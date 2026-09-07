package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// NewHandler serves /metrics (Prometheus exposition format) and /healthz
// (200 if check succeeds, 503 otherwise). Both are intentionally
// unauthenticated — this is meant to run on its own private listener
// (listen.metrics), scraped from inside the deployment network, not
// exposed through Traefik alongside the webmail UI.
func NewHandler(check func() error) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if check != nil {
			if err := check(); err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte("unhealthy: " + err.Error()))
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}
