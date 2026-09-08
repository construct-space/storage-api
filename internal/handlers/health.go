package handlers

import (
	"encoding/json"
	"net/http"
)

// Health returns 200 ok. CapRover health-check + load-balancer probe.
// We don't reach R2 here on purpose — a brief R2 hiccup must not flap
// the health check and cause CapRover to recycle the container.
func Health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":  "ok",
		"service": "storage-api",
	})
}
