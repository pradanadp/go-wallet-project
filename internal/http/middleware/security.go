package middleware

import (
	"crypto/subtle"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/example/wallet-service/pkg/apierror"
	"github.com/example/wallet-service/pkg/reqctx"
	"github.com/example/wallet-service/pkg/response"
)

// APIKeyHeader is the header clients use to send their credentials.
const APIKeyHeader = "X-API-Key"

// Auth is simple API key authentication.
//
// When apiKey is empty, authentication is disabled (development mode) and a
// warning is logged once at startup so it is not forgotten in production.
func Auth(apiKey string) Middleware {
	if apiKey == "" {
		slog.Warn("API key authentication is disabled (API_KEY is empty); do not use this in production")
		return func(next http.Handler) http.Handler { return next }
	}

	expected := []byte(apiKey)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			provided := []byte(r.Header.Get(APIKeyHeader))
			if subtle.ConstantTimeCompare(provided, expected) != 1 {
				w.Header().Set("WWW-Authenticate", `ApiKey realm="wallet-service"`)
				response.Error(w, r, apierror.Unauthorized("X-API-Key is missing or invalid"))
				return
			}
			subject := r.Header.Get("X-Client-Name")
			if subject == "" {
				subject = "api-key-client"
			}
			next.ServeHTTP(w, r.WithContext(reqctx.WithPrincipal(r.Context(), reqctx.Principal{Subject: subject})))
		})
	}
}

// RateLimit throttles requests per IP address using a token bucket.
//
// A token bucket is used (not a fixed window) because it allows a short, reasonable
// burst from mobile clients without violating the long-term average, and without
// the "2x limit at the window boundary" problem that fixed windows have.
//
// Limiters are stored per process (in memory), so the effective limit = RPS x replicas.
// A strict global limit would need a shared store (Redis) - out of scope here.
func RateLimit(rps float64, burst int) Middleware {
	if rps <= 0 || burst <= 0 {
		return func(next http.Handler) http.Handler { return next }
	}

	store := newLimiterStore(rps, burst)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !store.allow(clientIP(r), time.Now()) {
				w.Header().Set("Retry-After", "1")
				response.Error(w, r, apierror.RateLimited())
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// limiterStore keeps one limiter per IP with lazy cleanup: there is no background
// goroutine, so this middleware cannot leak resources.
type limiterStore struct {
	mu       sync.Mutex
	limiters map[string]*limiterEntry
	rps      float64
	burst    int
	requests uint64
}

type limiterEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

const (
	limiterIdleTTL    = 10 * time.Minute
	limiterSweepEvery = 1024
)

func newLimiterStore(rps float64, burst int) *limiterStore {
	return &limiterStore{
		limiters: make(map[string]*limiterEntry),
		rps:      rps,
		burst:    burst,
	}
}

func (s *limiterStore) allow(key string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.requests++
	if s.requests%limiterSweepEvery == 0 {
		for k, entry := range s.limiters {
			if now.Sub(entry.lastSeen) > limiterIdleTTL {
				delete(s.limiters, k)
			}
		}
	}

	entry, ok := s.limiters[key]
	if !ok {
		entry = &limiterEntry{limiter: rate.NewLimiter(rate.Limit(s.rps), s.burst)}
		s.limiters[key] = entry
	}
	entry.lastSeen = now

	return entry.limiter.AllowN(now, 1)
}

// clientIP determines the rate limit key.
//
// X-Forwarded-For is used first because the service is assumed to sit behind a
// trusted load balancer that overwrites it. When the service is exposed directly,
// this header can be spoofed by clients - set TRUST_PROXY=false in that case.
func clientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		// the first entry is the real client according to the proxy.
		if idx := strings.IndexByte(forwarded, ','); idx >= 0 {
			return strings.TrimSpace(forwarded[:idx])
		}
		return strings.TrimSpace(forwarded)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
