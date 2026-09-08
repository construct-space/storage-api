package config

import (
	"bufio"
	"os"
	"strings"
)

// Config is the storage-api runtime config. R2 credentials + bucket
// allowlist + the public base URL clients should use to GET objects.
type Config struct {
	Port           string
	AppURL         string
	AllowedOrigins []string

	// R2 (Cloudflare) — S3-compatible. Endpoint is computed from the
	// account id; override via R2_ENDPOINT for staging or migration.
	R2AccountID     string
	R2AccessKey     string
	R2SecretKey     string
	R2Endpoint      string // optional — defaults to https://<account>.r2.cloudflarestorage.com
	R2Region        string // R2 ignores region but the SDK requires one; "auto" works.
	R2PublicBase    string // public base URL used in returned object URLs (CDN, custom domain)
	R2DefaultBucket string

	// Per-bucket endpoint overrides. R2 has separate jurisdictions
	// (default, EU, FedRAMP) — each is a distinct S3 endpoint URL even
	// for the same account. A bucket created in the EU jurisdiction can
	// ONLY be reached via the `.eu.r2.cloudflarestorage.com` endpoint;
	// hitting it with the default endpoint returns NoSuchBucket.
	//
	// `BucketEndpoint["construct-space-data-eu"]` → the EU endpoint URL,
	// `BucketPublicBase["construct-space-data-eu"]` → the EU CDN base.
	// Buckets not in the maps fall through to R2Endpoint / R2PublicBase.
	BucketEndpoint   map[string]string
	BucketPublicBase map[string]string

	// AllowedBuckets is the comma-separated allowlist. Requests targeting a
	// bucket not in this set are rejected — keeps the service from being
	// hijacked into writing to arbitrary R2 buckets sharing the same key.
	AllowedBuckets []string

	// Per-upload byte cap (proxy-upload route). Presigned PUT bypasses this
	// — clients PUT directly to R2 and that's R2's quota.
	MaxUploadBytes int64

	// PresignTTLSeconds — how long a presigned PUT URL stays valid.
	PresignTTLSeconds int

	// Service-to-service shared secret. Required on /internal/* routes.
	InternalSecret string

	// EnforceReadScope, when true, requires GET /api/storage/* callers to be
	// gateway-authenticated and limited to their own key prefix (mirrors the
	// write-scope rule). Default false to avoid breaking any public-asset read
	// flow that still goes through the proxy — flip STORAGE_ENFORCE_READ_AUTH
	// =true once public assets are confirmed to be served from the R2 CDN base
	// rather than this endpoint. See SECURITY note in handlers/storage.go.
	EnforceReadScope bool
}

func Load() *Config {
	loadEnvFile(".env")

	origins := splitCSV(env("ALLOWED_ORIGINS", "https://my.lisaos.dev,tauri://localhost,http://localhost:3000"))
	defaultBucket := env("R2_DEFAULT_BUCKET", env("STORAGE_BUCKET", env("R2_BUCKET", "")))
	buckets := splitCSV(env("ALLOWED_BUCKETS", defaultBucket))

	endpoint := env("R2_ENDPOINT", "")
	if endpoint == "" {
		if id := env("R2_ACCOUNT_ID", ""); id != "" {
			endpoint = "https://" + id + ".r2.cloudflarestorage.com"
		}
	}

	// Jurisdiction-suffixed overrides. Convention is to suffix the env
	// key with `_<JURISDICTION>` and list which buckets live there as a
	// CSV in `R2_BUCKETS_<JURISDICTION>`. EU is the only non-default
	// jurisdiction in current use; FedRAMP would be added the same way.
	bucketEndpoint := map[string]string{}
	bucketPublicBase := map[string]string{}
	for _, j := range []string{"EU", "FEDRAMP"} {
		ep := env("R2_ENDPOINT_"+j, "")
		// Auto-derive from account_id when only the jurisdiction infix is
		// known — saves operators from hand-pasting the full endpoint.
		if ep == "" {
			if id := env("R2_ACCOUNT_ID", ""); id != "" {
				if j == "EU" {
					ep = "https://" + id + ".eu.r2.cloudflarestorage.com"
				}
			}
		}
		pub := env("R2_PUBLIC_BASE_URL_"+j, "")
		if ep == "" && pub == "" {
			continue
		}
		for _, b := range splitCSV(env("R2_BUCKETS_"+j, "")) {
			if ep != "" {
				bucketEndpoint[b] = ep
			}
			if pub != "" {
				bucketPublicBase[b] = pub
			}
		}
	}

	return &Config{
		Port:           env("PORT", "8000"),
		AppURL:         env("APP_URL", "http://localhost:8000"),
		AllowedOrigins: origins,

		R2AccountID:     env("R2_ACCOUNT_ID", ""),
		R2AccessKey:     env("R2_ACCESS_KEY_ID", ""),
		R2SecretKey:     env("R2_SECRET_ACCESS_KEY", ""),
		R2Endpoint:      endpoint,
		R2Region:        env("R2_REGION", "auto"),
		R2PublicBase:    env("R2_PUBLIC_BASE_URL", ""),
		R2DefaultBucket: defaultBucket,

		BucketEndpoint:   bucketEndpoint,
		BucketPublicBase: bucketPublicBase,

		AllowedBuckets:    buckets,
		MaxUploadBytes:    int64(envInt("MAX_UPLOAD_BYTES", 10*1024*1024)), // 10 MB default for proxy
		PresignTTLSeconds: envInt("PRESIGN_TTL_SECONDS", 900),              // 15 min default

		InternalSecret:   env("INTERNAL_SHARED_SECRET", ""),
		EnforceReadScope: env("STORAGE_ENFORCE_READ_AUTH", "") == "true",
	}
}

// EndpointForBucket returns the R2 endpoint URL to use when talking
// about `bucket`. Falls back to the default `R2Endpoint` when the
// bucket isn't in the per-bucket override map.
func (c *Config) EndpointForBucket(bucket string) string {
	if ep, ok := c.BucketEndpoint[bucket]; ok && ep != "" {
		return ep
	}
	return c.R2Endpoint
}

// PublicBaseForBucket returns the CDN base URL for `bucket`, or the
// default base when no override is set.
func (c *Config) PublicBaseForBucket(bucket string) string {
	if pub, ok := c.BucketPublicBase[bucket]; ok && pub != "" {
		return pub
	}
	return c.R2PublicBase
}

// BucketAllowed reports whether a bucket name is in the allowlist.
func (c *Config) BucketAllowed(name string) bool {
	for _, b := range c.AllowedBuckets {
		if b == name {
			return true
		}
	}
	return false
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := env(key, "")
	if v == "" {
		return fallback
	}
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return fallback
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func loadEnvFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if os.Getenv(key) == "" {
			_ = os.Setenv(key, val)
		}
	}
}
