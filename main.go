package main

import (
	"log"
	"net/http"
	"time"

	"construct/storage/internal/config"
	"construct/storage/internal/handlers"
	"construct/storage/internal/middleware"
	"construct/storage/internal/r2"
)

func main() {
	cfg := config.Load()
	handlers.Cfg = cfg

	client, err := r2.New(cfg)
	if err != nil {
		log.Fatalf("init r2: %v", err)
	}
	handlers.R2 = client

	mux := http.NewServeMux()

	// Health (public)
	mux.HandleFunc("GET /health", handlers.Health)
	mux.HandleFunc("GET /api/health", handlers.Health)

	// Public storage surface — read + write through presign / proxy.
	// Writes still need credentials at the application layer; today
	// we gate via the same gateway-secret as /internal/* (callers
	// either pass through the my.c.s gateway or hold the secret
	// directly). When a publisher-key path lands, this gets a second
	// auth lane.
	mux.Handle("POST /api/storage/presign", middleware.AdminAuth(http.HandlerFunc(handlers.PresignUpload)))
	mux.Handle("POST /api/storage/upload", middleware.AdminAuth(http.HandlerFunc(handlers.ProxyUpload)))
	mux.HandleFunc("GET /api/storage/{bucket}/{key...}", handlers.GetObject)
	// Public DELETE — session-auth (gateway-trusted + user identity).
	// Reuses the same handler as the /internal route; difference is the
	// auth lane. Browsers call this through the gateway with their
	// session; admin tools / services still use /internal with the
	// shared secret.
	mux.Handle("DELETE /api/storage/{bucket}/{key...}", middleware.SessionAuth(http.HandlerFunc(handlers.DeleteObject)))

	// Internal — service-to-service. Gateway secret only.
	mux.Handle("DELETE /internal/storage/{bucket}/{key...}", middleware.AdminAuth(http.HandlerFunc(handlers.DeleteObject)))
	mux.Handle("POST /internal/storage/copy", middleware.AdminAuth(http.HandlerFunc(handlers.CopyObject)))
	mux.Handle("GET /internal/storage/{bucket}/list", middleware.AdminAuth(http.HandlerFunc(handlers.ListObjects)))

	handler := middleware.Logger(middleware.CORS(cfg)(mux))

	if !cfg.EnforceReadScope {
		log.Printf("WARNING: GET /api/storage/* is UNAUTHENTICATED — any caller can read any object by key. Set STORAGE_ENFORCE_READ_AUTH=true once public assets are confirmed served from the R2 CDN base, not this proxy.")
	}
	log.Printf("storage-api listening on :%s (buckets: %v)", cfg.Port, cfg.AllowedBuckets)
	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
