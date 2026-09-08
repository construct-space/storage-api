// SessionAuth — the public-storage auth lane.
//
// The gateway (my.lisaos.dev nginx) auth_requests the caller's
// session against accounts before proxying. On success it writes the
// usual X-Auth-User-* headers + signs the request with the shared
// X-Internal-Secret. SessionAuth verifies both: gateway-trusted AND
// a user-id is present. That's the contract every "user does X to
// their own data" route needs — uploads, deletes, signed GETs.
//
// AdminAuth (internal secret only, no user identity required) stays
// for service-to-service calls. SessionAuth is the right gate for any
// route reachable through `/api/storage/*` from a browser.

package middleware

import (
	"net/http"

	"construct/storage/internal/gwauth"
)

func SessionAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := gwauth.Gateway(r)
		if id == nil || id.UserID == "" {
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
