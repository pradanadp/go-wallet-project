package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/example/wallet-service/pkg/apierror"
	"github.com/example/wallet-service/pkg/reqctx"
	"github.com/example/wallet-service/pkg/response"
)

// RequestIDHeader is the name of the request id header that is accepted and sent.
const RequestIDHeader = "X-Request-Id"

// RequestID ensures every request has an id that can be correlated in logs.
// A client-supplied id (X-Request-Id) is honoured when present so cross-service
// tracing stays connected.
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := sanitizeRequestID(r.Header.Get(RequestIDHeader))
			if id == "" {
				id = newRequestID()
			}
			w.Header().Set(RequestIDHeader, id)
			next.ServeHTTP(w, r.WithContext(reqctx.WithRequestID(r.Context(), id)))
		})
	}
}

// Logger writes one structured log line per request (after the handler returns),
// containing method, route pattern, status, duration, response size, and error.
//
// This middleware sits innermost (next to the handler) so that
// http.Request.Pattern has already been filled in by ServeMux and can be used as a
// low-cardinality route label.
func Logger(base *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			requestID := reqctx.RequestID(r.Context())

			logger := base.With(
				slog.String("request_id", requestID),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
			)
			recorder := &responseRecorder{ResponseWriter: w}

			next.ServeHTTP(recorder, r.WithContext(reqctx.WithLogger(r.Context(), logger)))

			duration := time.Since(start)
			status := recorder.Status()
			attrs := []slog.Attr{
				slog.String("route", routeLabel(r)),
				slog.Int("status", status),
				slog.Duration("duration", duration),
				slog.Int64("bytes", recorder.bytes),
			}
			if remote := clientIP(r); remote != "" {
				attrs = append(attrs, slog.String("client_ip", remote))
			}

			switch {
			case status >= http.StatusInternalServerError:
				logger.LogAttrs(r.Context(), slog.LevelError, "request completed", attrs...)
			case status >= http.StatusBadRequest:
				logger.LogAttrs(r.Context(), slog.LevelWarn, "request completed", attrs...)
			default:
				logger.LogAttrs(r.Context(), slog.LevelInfo, "request completed", attrs...)
			}
		})
	}
}

// Recover turns a handler panic into a 500 problem details response, so one bad
// request cannot bring down the whole server process.
func Recover() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if recovered := recover(); recovered != nil {
					if recovered == http.ErrAbortHandler {
						panic(recovered) // net/http contract: let it propagate
					}
					reqctx.Logger(r.Context()).Error("panic recovered",
						slog.Any("panic", recovered),
						slog.String("stack", string(debug.Stack())),
						slog.String("route", routeLabel(r)),
					)
					response.Error(w, r, apierror.InternalPanic())
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// Timeout caps the total request processing time. This limit matters for
// database-touching operations: a cancelled context cancels the query, so a
// transaction does not hold locks longer than it should.
func Timeout(d time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failed: continue without a request id rather than rejecting the request.
		return ""
	}
	return hex.EncodeToString(b[:])
}

// sanitizeRequestID prevents log injection and odd client-supplied values.
func sanitizeRequestID(raw string) string {
	const maxLen = 128
	if len(raw) == 0 || len(raw) > maxLen {
		return ""
	}
	for _, r := range raw {
		if r < 0x21 || r > 0x7e {
			return ""
		}
	}
	return raw
}
