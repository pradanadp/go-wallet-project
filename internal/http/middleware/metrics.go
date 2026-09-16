package middleware

import (
	"net/http"
	"strconv"
	"time"

	"github.com/example/wallet-service/internal/platform/metrics"
)

// Metrics records HTTP metrics: request count per (method, route, status),
// the latency distribution, and the number of in-flight requests.
//
// The route uses the ServeMux pattern (not the raw URL) so the label count stays
// bounded instead of exploding on paths that contain ids.
func Metrics(m *metrics.Metrics) Middleware {
	if m == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m.IncInFlight()
			defer m.DecInFlight()

			recorder := &responseRecorder{ResponseWriter: w}
			start := time.Now()

			next.ServeHTTP(recorder, r)

			m.ObserveHTTP(r.Method, routeLabel(r), strconv.Itoa(recorder.Status()), time.Since(start))
		})
	}
}
