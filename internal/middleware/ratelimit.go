package middleware

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

type rateBucket struct {
	count    int
	resetAt  time.Time
}

var (
	rateMu      sync.Mutex
	rateBuckets = make(map[string]*rateBucket)
)

// RateLimit applies per-IP rate limiting to sensitive auth endpoints.
// 10 attempts per minute for login/register/forgot-password/2fa.
func RateLimit(next http.Handler) http.Handler {
	// Clean up stale entries periodically
	go func() {
		for {
			time.Sleep(5 * time.Minute)
			rateMu.Lock()
			now := time.Now()
			for k, b := range rateBuckets {
				if now.After(b.resetAt) {
					delete(rateBuckets, k)
				}
			}
			rateMu.Unlock()
		}
	}()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			next.ServeHTTP(w, r)
			return
		}

		path := r.URL.Path
		limited := path == "/api/auth/login" || path == "/api/auth/register" ||
			path == "/forgot-password" || path == "/reset-password" ||
			path == "/verify-2fa" || path == "/api/2fa/verify"

		if !limited {
			next.ServeHTTP(w, r)
			return
		}

		ip := r.Header.Get("X-Forwarded-For")
		if ip != "" {
			ip = strings.TrimSpace(strings.Split(ip, ",")[0])
		} else if rip := r.Header.Get("X-Real-IP"); rip != "" {
			ip = rip
		} else {
			ip = r.RemoteAddr
			// Strip port from RemoteAddr (e.g. "127.0.0.1:54321" -> "127.0.0.1")
			if idx := strings.LastIndex(ip, ":"); idx > 0 {
				ip = ip[:idx]
			}
		}
		key := ip + ":" + path

		rateMu.Lock()
		bucket, exists := rateBuckets[key]
		now := time.Now()
		if !exists || now.After(bucket.resetAt) {
			bucket = &rateBucket{count: 0, resetAt: now.Add(1 * time.Minute)}
			rateBuckets[key] = bucket
		}
		bucket.count++
		count := bucket.count
		rateMu.Unlock()

		if count > 10 {
			http.Error(w, "Too many requests. Please try again later.", http.StatusTooManyRequests)
			return
		}

		next.ServeHTTP(w, r)
	})
}
