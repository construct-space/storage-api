// Package gwauth is the gateway-trust helper. When my.lisaos.dev (the
// nginx gateway) proxies a request to a downstream service, it validates
// the caller's token against accounts, writes X-Auth-* headers describing
// the identity, and signs the request with X-Internal-Secret (shared via
// INTERNAL_SHARED_SECRET env var).
//
// Gateway(r) is the one-call check: "did the gateway authenticate this
// caller?" Returns a decoded Identity on success, nil otherwise. The
// service can then skip its own token validation for gateway-trusted
// requests.
//
// Small on purpose — every service has richer auth for sessions, CLI
// tokens, publisher keys. What they share is "trust the gateway when it
// proves who it is." Lives inline (one .go file) in each service that
// needs it rather than as a shared module — keeps each repo standalone
// and side-steps private-module Docker-auth headaches.
package gwauth

import (
	"net/http"
	"os"
	"strings"
)

// Identity is the decoded identity the gateway has asserted on a request.
// All fields map directly to the X-Auth-* headers the gateway wrote.
type Identity struct {
	UserID      string   // X-Auth-User-ID (user UUID)
	Email       string   // X-Auth-User-Email
	Name        string   // X-Auth-User-Name
	Scope       string   // X-Auth-Scope: "user" | "org"
	OrgID       string   // X-Auth-Org-ID (when Scope == "org")
	OrgSlug     string   // X-Auth-Org-Slug (when Scope == "org")
	Roles       []string // X-Auth-Roles, comma-separated → slice
	PublisherID string   // X-Auth-Publisher-ID (when a csk_live_* key was presented)
}

// HasRole reports whether the caller holds the named role in their current
// org scope. Returns false for personal scope or when the role isn't set.
func (id *Identity) HasRole(role string) bool {
	if id == nil || id.Scope != "org" {
		return false
	}
	for _, r := range id.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// Trusted reports whether the request came through the gateway. One string
// compare, no side effects. Use it when you only need the boolean; use
// Gateway() when you also want the decoded identity.
//
// The secret is read from INTERNAL_SHARED_SECRET at call time — changes
// take effect on the next request without a restart.
func Trusted(r *http.Request) bool {
	secret := os.Getenv("INTERNAL_SHARED_SECRET")
	if secret == "" {
		return false
	}
	return r.Header.Get("X-Internal-Secret") == secret
}

// Gateway returns the gateway-asserted identity on the request, or nil if
// the request didn't come through the gateway (missing or mismatched
// X-Internal-Secret, or no X-Auth-User-ID header). Services use this as
// the first check in their auth chain: if non-nil, the caller is
// authenticated and no further validation is required.
func Gateway(r *http.Request) *Identity {
	if !Trusted(r) {
		return nil
	}
	userID := r.Header.Get("X-Auth-User-ID")
	if userID == "" {
		return nil
	}

	id := &Identity{
		UserID:      userID,
		Email:       r.Header.Get("X-Auth-User-Email"),
		Name:        r.Header.Get("X-Auth-User-Name"),
		Scope:       r.Header.Get("X-Auth-Scope"),
		OrgID:       r.Header.Get("X-Auth-Org-ID"),
		OrgSlug:     r.Header.Get("X-Auth-Org-Slug"),
		PublisherID: r.Header.Get("X-Auth-Publisher-ID"),
	}
	if roles := r.Header.Get("X-Auth-Roles"); roles != "" {
		id.Roles = strings.Split(roles, ",")
	}
	return id
}
