package handler

import (
	"net/http"

	"github.com/example/wallet-service/internal/http/middleware"
	"github.com/example/wallet-service/pkg/apierror"
	"github.com/example/wallet-service/pkg/response"
)

// Dependencies holds every handler registered on the router.
type Dependencies struct {
	Wallets   *WalletHandler
	Movements *MovementHandler
	Health    *HealthHandler
	Metrics   http.Handler
}

// route binds a ServeMux method + pattern to a handler.
//
// The pattern uses the built-in Go 1.22+ routing (`METHOD /path/{param}`), so there
// is no third-party router: route selection, wildcards, and pattern conflicts are
// handled by the standard library.
type route struct {
	method  string
	pattern string
	handler http.HandlerFunc
}

// NewRouter assembles every route.
//
// Each handler is wrapped in the middleware chain at registration time (not once
// outside the mux). As a consequence the innermost middleware can read
// http.Request.Pattern, which is used as the route label in logs and metrics so
// its cardinality stays bounded.
func NewRouter(deps Dependencies, middlewares ...middleware.Middleware) http.Handler {
	mux := http.NewServeMux()
	chain := func(h http.HandlerFunc) http.Handler {
		return middleware.Chain(h, middlewares...)
	}

	routes := []route{
		{method: http.MethodPost, pattern: "/v1/wallets", handler: deps.Wallets.Create},
		{method: http.MethodGet, pattern: "/v1/wallets/{walletID}", handler: deps.Wallets.Get},
		{method: http.MethodGet, pattern: "/v1/wallets/{walletID}/transactions", handler: deps.Wallets.ListTransactions},
		{method: http.MethodPost, pattern: "/v1/wallets/{walletID}/topup", handler: deps.Movements.TopUp},
		{method: http.MethodPost, pattern: "/v1/wallets/{walletID}/withdraw", handler: deps.Movements.Withdraw},
		{method: http.MethodPost, pattern: "/v1/wallets/{walletID}/transfer", handler: deps.Movements.Transfer},
		{method: http.MethodGet, pattern: "/healthz", handler: deps.Health.Live},
		{method: http.MethodGet, pattern: "/readyz", handler: deps.Health.Ready},
	}

	// ServeMux replies with 405 and a plain text body when the path matches but the
	// method is not registered. To keep the "every error is RFC 7807" contract,
	// a path-only pattern is registered as a fallback: `METHOD /x` is more specific
	// so it still wins for valid methods, while other methods fall through to our
	// 405 handler.
	methodFallbacks := make(map[string]struct{}, len(routes))
	for _, rt := range routes {
		mux.Handle(rt.method+" "+rt.pattern, chain(rt.handler))

		if _, ok := methodFallbacks[rt.pattern]; ok {
			continue
		}
		methodFallbacks[rt.pattern] = struct{}{}
		mux.Handle(rt.pattern, chain(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Allow", allowedMethods(routes, r.Pattern))
			response.Error(w, r, apierror.MethodNotAllowed(r.Method))
		}))
	}

	// /metrics is deliberately NOT wrapped in middleware: it targets an internal
	// scraper, so it needs no application rate limit/auth (restrict it at the network layer).
	if deps.Metrics != nil {
		mux.Handle("GET /metrics", deps.Metrics)
		mux.Handle("/metrics", chain(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Allow", http.MethodGet)
			response.Error(w, r, apierror.MethodNotAllowed(r.Method))
		}))
	}

	// Every other path: 404 in problem details format.
	mux.Handle("/", chain(func(w http.ResponseWriter, r *http.Request) {
		response.Error(w, r, apierror.NotFoundEndpoint(r.URL.Path))
	}))

	return mux
}

// allowedMethods collects the valid methods for a pattern (for the Allow header).
// An empty pattern means the fallback ran without a matching route.
func allowedMethods(routes []route, pattern string) string {
	methods := make([]string, 0, 2)
	for _, rt := range routes {
		if rt.pattern != pattern {
			continue
		}
		methods = append(methods, rt.method)
	}
	if len(methods) == 0 {
		return http.MethodGet + ", " + http.MethodPost
	}
	allowed := methods[0]
	for _, m := range methods[1:] {
		allowed += ", " + m
	}
	return allowed
}
