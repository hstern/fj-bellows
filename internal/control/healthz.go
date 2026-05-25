package control

import (
	"encoding/json"
	"net/http"
	"time"
)

// plainHealthz is the k8s-style HTTP shim for liveness/readiness probes that
// don't speak ConnectRPC. Returns 200 + a tiny JSON body when the backend
// reports healthy, 503 otherwise.
func plainHealthz(b Backend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := b.Health(r.Context())
		w.Header().Set("Content-Type", "application/json")
		if !s.Healthy {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"healthy":               s.Healthy,
			"last_tick_at":          orNil(s.LastTickAt),
			"last_provider_list_at": orNil(s.LastProviderListAt),
			"last_forgejo_poll_at":  orNil(s.LastForgejoPollAt),
		})
	}
}

func orNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
