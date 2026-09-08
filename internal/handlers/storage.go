// storage.go — public + service-to-service routes. Public reads can
// resolve to a presigned URL or stream-through; writes are gated by
// X-Internal-Secret (for service-to-service) or X-API-Key (for
// publishers) and produce a presigned PUT for direct upload.
package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"

	"construct/storage/internal/config"
	"construct/storage/internal/r2"
)

// Cfg + R2 are wired from main.go on startup.
var (
	Cfg *config.Config
	R2  *r2.Client
)

// PresignUpload — POST /api/storage/presign
//
// Body: { bucket, key, content_type? }
// Returns: { url, public_url, expires_at, key, bucket }
//
// Caller PUTs the bytes to `url` directly with the same content_type.
// Returned `public_url` is what to store in your db / show to users.
func PresignUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body struct {
		Bucket      string `json:"bucket"`
		Key         string `json:"key"`
		ContentType string `json:"content_type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "invalid body"})
		return
	}
	if body.Bucket == "" {
		body.Bucket = Cfg.R2DefaultBucket
	}
	if !Cfg.BucketAllowed(body.Bucket) {
		writeJSON(w, 400, map[string]any{"error": "bucket not allowed: " + body.Bucket})
		return
	}
	if body.Key == "" {
		// Generate a random key under a date-prefixed path. Callers can
		// override with their own key (e.g. <space-id>/<version>.tgz).
		body.Key = autoKey()
	}
	body.Key = scopeKeyForGatewayUser(r, body.Key)
	if !validKey(body.Key) {
		writeJSON(w, 400, map[string]any{"error": "invalid key: must not start with / or contain ../"})
		return
	}
	if !gatewayUserMayWriteKey(r, body.Key) {
		writeJSON(w, 403, map[string]any{"error": "key is outside the authenticated user's storage scope"})
		return
	}
	signed, expires, err := R2.PresignPut(r.Context(), body.Bucket, body.Key, body.ContentType)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{
		"url":        signed,
		"public_url": R2.PublicURL(body.Bucket, body.Key),
		"expires_at": expires.UTC().Format("2006-01-02T15:04:05Z"),
		"key":        body.Key,
		"bucket":     body.Bucket,
	})
}

// ProxyUpload — POST /api/storage/upload
//
// Multipart form: file=<binary>, bucket?, key?
// For files <= MaxUploadBytes; everything else uses presign + direct PUT.
//
// Useful for tiny payloads where setting up a presigned PUT roundtrip
// adds more latency than the upload itself (icons, logos, single-image
// overrides). Returns { public_url, key, bucket, size }.
func ProxyUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(Cfg.MaxUploadBytes + 1024); err != nil {
		writeJSON(w, 400, map[string]any{"error": "parse multipart: " + err.Error()})
		return
	}

	bucket := r.FormValue("bucket")
	if bucket == "" {
		bucket = Cfg.R2DefaultBucket
	}
	if !Cfg.BucketAllowed(bucket) {
		writeJSON(w, 400, map[string]any{"error": "bucket not allowed: " + bucket})
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": "file field required"})
		return
	}
	defer file.Close()

	if header.Size > Cfg.MaxUploadBytes {
		writeJSON(w, 413, map[string]any{
			"error":    "file too large for proxy upload — use /api/storage/presign instead",
			"max_size": Cfg.MaxUploadBytes,
		})
		return
	}

	key := r.FormValue("key")
	if key == "" {
		key = autoKey() + path.Ext(header.Filename)
	}
	key = scopeKeyForGatewayUser(r, key)
	if !validKey(key) {
		writeJSON(w, 400, map[string]any{"error": "invalid key"})
		return
	}
	if !gatewayUserMayWriteKey(r, key) {
		writeJSON(w, 403, map[string]any{"error": "key is outside the authenticated user's storage scope"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(file, Cfg.MaxUploadBytes+1))
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": "read upload: " + err.Error()})
		return
	}
	contentType := header.Header.Get("Content-Type")
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = http.DetectContentType(body)
	}
	if err := R2.PutObject(r.Context(), bucket, key, contentType, body); err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 201, map[string]any{
		"public_url": R2.PublicURL(bucket, key),
		"key":        key,
		"bucket":     bucket,
		"size":       len(body),
	})
}

// GetObject — GET /api/storage/{bucket}/{key:...}
//
// Two modes, controlled by ?mode=:
//
//	mode=redirect (default) — 302 to a 5-minute presigned GET URL.
//	                           Cheap; offloads bytes to R2 / CDN.
//	mode=stream             — proxy-stream the bytes through this server.
//	                           Use when downstream callers can't follow redirects.
func GetObject(w http.ResponseWriter, r *http.Request) {
	bucket := r.PathValue("bucket")
	key := r.PathValue("key")
	if !Cfg.BucketAllowed(bucket) {
		writeJSON(w, 400, map[string]any{"error": "bucket not allowed"})
		return
	}
	if !validKey(key) {
		writeJSON(w, 400, map[string]any{"error": "invalid key"})
		return
	}
	// Symmetry with writes: PresignUpload / ProxyUpload run the caller's key
	// through scopeKeyForGatewayUser, which prepends `users/<uid>/` for keys
	// outside the caller's own prefixes. Reads MUST apply the same scoping,
	// otherwise a gateway caller asking for "drive/x" looks up the unscoped
	// key while the bytes actually live at "users/<uid>/drive/x" — and with
	// EnforceReadScope on, the bare key is never inside the caller's prefix so
	// every space read 403s. Scope first, then enforce. Public (non-gateway)
	// reads have no user id, so scopeKeyForGatewayUser leaves their key as-is.
	if gatewayUserID(r) != "" {
		key = scopeKeyForGatewayUser(r, key)
	}
	// SECURITY: this route has no auth middleware, so without a scope check
	// anyone can read any object by key (keys are user-UUID-prefixed and
	// predictable). When EnforceReadScope is on, require a gateway-
	// authenticated caller limited to their own prefix — same rule as writes.
	// Default off so a public-asset read flow through this proxy isn't broken
	// blind; flip STORAGE_ENFORCE_READ_AUTH=true once confirmed safe.
	if Cfg.EnforceReadScope {
		if gatewayUserID(r) == "" || !gatewayUserMayWriteKey(r, key) {
			writeJSON(w, 403, map[string]any{"error": "forbidden"})
			return
		}
	}

	switch r.URL.Query().Get("mode") {
	case "stream":
		out, err := R2.GetObjectStream(r.Context(), bucket, key)
		if err != nil {
			writeJSON(w, 404, map[string]any{"error": "not found"})
			return
		}
		defer out.Body.Close()
		if out.ContentType != nil {
			w.Header().Set("Content-Type", *out.ContentType)
		}
		if out.ContentLength != nil {
			w.Header().Set("Content-Length", itoa(*out.ContentLength))
		}
		_, _ = io.Copy(w, out.Body)
	default:
		signed, err := R2.PresignGet(r.Context(), bucket, key, 0)
		if err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
		http.Redirect(w, r, signed, http.StatusFound)
	}
}

// DeleteObject — DELETE /internal/storage/{bucket}/{key:...}
// Caller is gated by X-Internal-Secret in middleware. Idempotent.
func DeleteObject(w http.ResponseWriter, r *http.Request) {
	bucket := r.PathValue("bucket")
	key := r.PathValue("key")
	if !Cfg.BucketAllowed(bucket) {
		writeJSON(w, 400, map[string]any{"error": "bucket not allowed"})
		return
	}
	if !validKey(key) {
		writeJSON(w, 400, map[string]any{"error": "invalid key"})
		return
	}
	if err := R2.DeleteObject(r.Context(), bucket, key); err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "deleted": bucket + "/" + key})
}

// CopyObject — POST /internal/storage/copy
// Body: { src_bucket, src_key, dst_bucket, dst_key }
func CopyObject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SrcBucket string `json:"src_bucket"`
		SrcKey    string `json:"src_key"`
		DstBucket string `json:"dst_bucket"`
		DstKey    string `json:"dst_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "invalid body"})
		return
	}
	if !Cfg.BucketAllowed(body.SrcBucket) || !Cfg.BucketAllowed(body.DstBucket) {
		writeJSON(w, 400, map[string]any{"error": "bucket not allowed"})
		return
	}
	if !validKey(body.SrcKey) || !validKey(body.DstKey) {
		writeJSON(w, 400, map[string]any{"error": "invalid key"})
		return
	}
	if err := R2.CopyObject(r.Context(), body.SrcBucket, body.SrcKey, body.DstBucket, body.DstKey); err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok":         true,
		"public_url": R2.PublicURL(body.DstBucket, body.DstKey),
	})
}

// ListObjects — GET /internal/storage/{bucket}/list?prefix=...
func ListObjects(w http.ResponseWriter, r *http.Request) {
	bucket := r.PathValue("bucket")
	if !Cfg.BucketAllowed(bucket) {
		writeJSON(w, 400, map[string]any{"error": "bucket not allowed"})
		return
	}
	prefix := r.URL.Query().Get("prefix")
	keys, err := R2.ListObjects(r.Context(), bucket, prefix)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"bucket": bucket, "prefix": prefix, "keys": keys})
}

// helpers

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// validKey rejects keys that try path-escape (../) or absolute (/).
// Other characters are fine — callers may construct meaningful paths.
func validKey(k string) bool {
	if k == "" {
		return false
	}
	if strings.HasPrefix(k, "/") {
		return false
	}
	for _, seg := range strings.Split(k, "/") {
		if seg == ".." || seg == "." {
			return false
		}
	}
	// Defense-in-depth — block anything that looks like a URL-decoded escape.
	if decoded, err := url.PathUnescape(k); err == nil && decoded != k {
		// Caller shouldn't send pre-encoded keys; we store the literal value.
		// Reject so the bucket layout matches what the caller chose.
		return false
	}
	return true
}

func gatewayUserID(r *http.Request) string {
	if r.Header.Get("X-Internal-Secret") == "" {
		return ""
	}
	return strings.TrimSpace(r.Header.Get("X-Auth-User-ID"))
}

// gatewayOrgID is the org the gateway asserts the caller is currently acting
// within (X-Auth-Org-ID, only set when X-Auth-Scope == "org"). The gateway
// only emits this header after verifying the user's membership in that org,
// so a non-empty value is a trusted "this caller belongs to this org" signal
// — enough to authorize the shared `orgs/<orgId>/` keyspace below.
func gatewayOrgID(r *http.Request) string {
	if r.Header.Get("X-Internal-Secret") == "" {
		return ""
	}
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Auth-Scope")), "org") {
		return ""
	}
	return strings.TrimSpace(r.Header.Get("X-Auth-Org-ID"))
}

func scopeKeyForGatewayUser(r *http.Request, key string) string {
	userID := gatewayUserID(r)
	if userID == "" {
		return key
	}
	key = strings.TrimLeft(key, "/")
	if key == "" {
		return "users/" + userID + "/" + autoKey()
	}
	if gatewayUserMayWriteKey(r, key) {
		return key
	}
	return "users/" + userID + "/" + key
}

func gatewayUserMayWriteKey(r *http.Request, key string) bool {
	userID := gatewayUserID(r)
	if userID == "" {
		return true
	}
	key = strings.TrimLeft(key, "/")
	allowedPrefixes := []string{
		userID + "/",
		"users/" + userID + "/",
		"user/" + userID + "/",
		"profiles/" + userID + "/",
		"profile/" + userID + "/",
		"attachments/" + userID + "/",
	}
	// Shared org keyspace: members operating in an org may read+write
	// `orgs/<orgId>/...` for the org the gateway verified them into. This is
	// what lets every member of a team see the same files (e.g. a shared
	// Drive) instead of each writing into their own `users/<uid>/` silo.
	if orgID := gatewayOrgID(r); orgID != "" {
		allowedPrefixes = append(allowedPrefixes,
			"orgs/"+orgID+"/",
			"org/"+orgID+"/",
		)
	}
	for _, prefix := range allowedPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// autoKey generates a date-prefixed random key. Used when caller didn't
// supply one. Format: 2026/04/29/abcdef1234567890.
func autoKey() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "auto/" + hex.EncodeToString(b)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
