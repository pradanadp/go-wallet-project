// Package middleware contains HTTP middleware built purely on the standard library.
//
// The middleware here is intentionally simple (func(http.Handler) http.Handler) so
// no framework is needed: composition is done by Chain() below.
package middleware

import (
	"net/http"
	"sync/atomic"
)

// Middleware is a function that wraps a handler.
type Middleware func(http.Handler) http.Handler

// Chain applies middleware in order: Chain(h, A, B) means the request goes
// through A first, then B, then h.
func Chain(h http.Handler, middlewares ...Middleware) http.Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		h = middlewares[i](h)
	}
	return h
}

// responseRecorder records the status code and byte count for logging/metrics
// without buffering the whole body in memory.
type responseRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (r *responseRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.status = status
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	atomic.AddInt64(&r.bytes, int64(n))
	return n, err
}

// Flush forwards Flush when the underlying ResponseWriter supports it (e.g. streaming).
func (r *responseRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Unwrap is used by http.ResponseController so optional features
// (Flush, Hijack, SetWriteDeadline) remain reachable.
func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *responseRecorder) Status() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

// routeLabel returns the matched route pattern, used as the metrics label.
//
// It uses http.Request.Pattern (Go 1.23+) so label cardinality stays bounded
// (route, not raw URL) and never explodes because of path parameters.
func routeLabel(r *http.Request) string {
	if r.Pattern == "" {
		return "unmatched"
	}
	return r.Pattern
}
