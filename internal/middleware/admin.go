package middleware

import (
	"net/http"

	"construct/storage/internal/gwauth"
)

// AdminAuth accepts requests carrying a matching X-Internal-Secret header.
// The secret is read from INTERNAL_SHARED_SECRET at call time.
func AdminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !gwauth.Trusted(r) {
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
