package middleware

import (
	"log"
	"net/http"
	"time"

	"construct/storage/internal/config"
)

// Logger emits one log line per request — method, path, duration.
func Logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

// CORS allows cross-origin reads + writes for storage-api. Public GET
// streaming routes need * because they may be embedded as <img> /
// fetch() from any consumer; presign/upload writes are stricter and
// only echo Origin when in AllowedOrigins.
func CORS(cfg *config.Config) func(http.Handler) http.Handler {
	allowed := make(map[string]bool)
	for _, o := range cfg.AllowedOrigins {
		allowed[o] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if r.Method == http.MethodGet {
				// Reads are public — let any origin embed them.
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else if allowed[origin] {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Internal-Secret")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
