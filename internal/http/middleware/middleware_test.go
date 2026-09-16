package middleware_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/example/wallet-service/internal/http/middleware"
	"github.com/example/wallet-service/internal/platform/metrics"
	"github.com/example/wallet-service/pkg/reqctx"
)

func TestRequestIDGeneratesAndPropagatesID(t *testing.T) {
	var captured string
	handler := middleware.Chain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			captured = reqctx.RequestID(r.Context())
			w.WriteHeader(http.StatusOK)
		}),
		middleware.RequestID(),
	)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	if captured == "" {
		t.Fatal("the request id was not injected into the context")
	}
	if header := recorder.Header().Get(middleware.RequestIDHeader); header != captured {
		t.Errorf("X-Request-Id header = %q, want %q", header, captured)
	}
}

func TestRequestIDHonoursClientHeaderAndRejectsJunk(t *testing.T) {
	cases := []struct {
		name       string
		incoming   string
		wantPrefix string
		wantSame   bool
	}{
		{name: "used as-is", incoming: "trace-abc-123", wantSame: true},
		{name: "too long is rejected", incoming: strings.Repeat("a", 200), wantSame: false},
		{name: "odd characters are rejected", incoming: "abc\ndef", wantSame: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var captured string
			handler := middleware.Chain(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					captured = reqctx.RequestID(r.Context())
				}),
				middleware.RequestID(),
			)

			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.Header.Set(middleware.RequestIDHeader, tc.incoming)
			handler.ServeHTTP(httptest.NewRecorder(), request)

			if tc.wantSame && captured != tc.incoming {
				t.Errorf("request id = %q, want %q", captured, tc.incoming)
			}
			if !tc.wantSame && (captured == tc.incoming || captured == "") {
				t.Errorf("request id = %q, it should have been regenerated", captured)
			}
		})
	}
}

func TestRecoverConvertsPanicToProblemDetails(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	handler := middleware.Chain(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic("connection dropped mid-flight")
		}),
		middleware.RequestID(),
		middleware.Logger(logger),
		middleware.Recover(),
	)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/wallets", nil))

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}

	var problem map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &problem); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if problem["code"] != "internal_error" {
		t.Errorf("code = %v, want internal_error", problem["code"])
	}
	if detail, _ := problem["detail"].(string); strings.Contains(detail, "connection dropped") {
		t.Error("internal detail leaked into the client response")
	}

	logOutput := buf.String()
	if !strings.Contains(logOutput, "panic recovered") {
		t.Errorf("the panic was not logged: %s", logOutput)
	}
	if strings.Contains(logOutput, "request completed") == false {
		t.Errorf("the panicking request was not logged as completed: %s", logOutput)
	}
}

func TestRecoverRethrowsAbortHandler(t *testing.T) {
	handler := middleware.Chain(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic(http.ErrAbortHandler)
		}),
		middleware.Recover(),
	)

	defer func() {
		if recovered := recover(); !errors.Is(recovered.(error), http.ErrAbortHandler) {
			t.Fatalf("panic = %v, want http.ErrAbortHandler to be re-raised", recovered)
		}
	}()
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestAuth(t *testing.T) {
	cases := []struct {
		name       string
		apiKey     string
		header     string
		wantStatus int
	}{
		{name: "disabled when API_KEY is empty", apiKey: "", header: "", wantStatus: http.StatusOK},
		{name: "correct key", apiKey: "secret-123", header: "secret-123", wantStatus: http.StatusOK},
		{name: "wrong key", apiKey: "secret-123", header: "wrong-key", wantStatus: http.StatusUnauthorized},
		{name: "key not sent", apiKey: "secret-123", header: "", wantStatus: http.StatusUnauthorized},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var principal string
			handler := middleware.Chain(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if p, ok := reqctx.PrincipalFrom(r.Context()); ok {
						principal = p.Subject
					}
				}),
				middleware.Auth(tc.apiKey),
			)

			request := httptest.NewRequest(http.MethodGet, "/v1/wallets", nil)
			if tc.header != "" {
				request.Header.Set(middleware.APIKeyHeader, tc.header)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)

			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			if tc.wantStatus == http.StatusUnauthorized {
				if recorder.Header().Get("WWW-Authenticate") == "" {
					t.Error("WWW-Authenticate header is missing")
				}
				if !strings.Contains(recorder.Body.String(), `"code":"unauthorized"`) {
					t.Errorf("body = %s", recorder.Body.String())
				}
			}
			if tc.wantStatus == http.StatusOK && tc.apiKey != "" && principal == "" {
				t.Error("principal was not set in the context after successful authentication")
			}
		})
	}
}

func TestRateLimitAllowsBurstThenRejects(t *testing.T) {
	handler := middleware.Chain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }),
		middleware.RateLimit(1, 2),
	)

	statuses := make([]int, 0, 4)
	for i := 0; i < 4; i++ {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.RemoteAddr = "203.0.113.7:12345"
		handler.ServeHTTP(recorder, request)
		statuses = append(statuses, recorder.Code)

		if recorder.Code == http.StatusTooManyRequests {
			if recorder.Header().Get("Retry-After") == "" {
				t.Error("Retry-After header is missing on 429")
			}
			if !strings.Contains(recorder.Body.String(), `"code":"rate_limited"`) {
				t.Errorf("body = %s", recorder.Body.String())
			}
		}
	}

	if statuses[0] != http.StatusOK || statuses[1] != http.StatusOK {
		t.Fatalf("the first two requests must pass (burst), got %v", statuses)
	}
	if statuses[2] != http.StatusTooManyRequests || statuses[3] != http.StatusTooManyRequests {
		t.Fatalf("the following requests must be 429, got %v", statuses)
	}
}

func TestRateLimitSeparatesClientsAndCanBeDisabled(t *testing.T) {
	handler := middleware.Chain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }),
		middleware.RateLimit(1, 1),
	)

	for _, ip := range []string{"198.51.100.1:1", "198.51.100.2:1"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.RemoteAddr = ip
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("the first request from %s must pass, got %d", ip, recorder.Code)
		}
	}

	// use X-Forwarded-For as the key.
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "10.0.0.1:1"
	request.Header.Set("X-Forwarded-For", "198.51.100.3, 10.0.0.9")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("a client with a different XFF must pass, got %d", recorder.Code)
	}

	disabled := middleware.Chain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }),
		middleware.RateLimit(0, 0),
	)
	for i := 0; i < 50; i++ {
		recorder := httptest.NewRecorder()
		disabled.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("rate limiting must be disabled, got %d", recorder.Code)
		}
	}
}

func TestLoggerRecordsRouteStatusAndRequestID(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	mux := http.NewServeMux()
	mux.Handle("GET /v1/wallets/{walletID}", middleware.Chain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = r.PathValue("walletID")
			w.WriteHeader(http.StatusTeapot)
			_, _ = w.Write([]byte("teapot"))
		}),
		middleware.RequestID(),
		middleware.Logger(logger),
	))

	request := httptest.NewRequest(http.MethodGet, "/v1/wallets/11111111-1111-4111-8111-111111111111", nil)
	mux.ServeHTTP(httptest.NewRecorder(), request)

	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &entry); err != nil {
		t.Fatalf("log is not JSON: %v (%s)", err, buf.String())
	}
	if entry["msg"] != "request completed" {
		t.Errorf("msg = %v", entry["msg"])
	}
	if entry["route"] != "GET /v1/wallets/{walletID}" {
		t.Errorf("route = %v (the label must use the ServeMux pattern, not the raw URL)", entry["route"])
	}
	if entry["status"] != float64(http.StatusTeapot) {
		t.Errorf("status = %v", entry["status"])
	}
	if entry["request_id"] == "" || entry["request_id"] == nil {
		t.Error("request_id is missing from the log")
	}
	if entry["bytes"] != float64(len("teapot")) {
		t.Errorf("bytes = %v", entry["bytes"])
	}
	if _, ok := entry["duration"]; !ok {
		t.Error("duration is missing from the log")
	}
}

func TestTimeoutCancelsContext(t *testing.T) {
	handler := middleware.Chain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
			if !errors.Is(context.Cause(r.Context()), context.DeadlineExceeded) {
				t.Errorf("cause = %v, want DeadlineExceeded", context.Cause(r.Context()))
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		}),
		middleware.Timeout(20*time.Millisecond),
	)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", recorder.Code)
	}
}

func TestMetricsMiddlewareRecordsRouteLabel(t *testing.T) {
	serviceMetrics := metrics.New(nil)

	mux := http.NewServeMux()
	mux.Handle("GET /healthz", middleware.Chain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }),
		middleware.Metrics(serviceMetrics),
	))

	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))

	expected := `
# HELP http_requests_total Total HTTP requests by method, route pattern, and status code.
# TYPE http_requests_total counter
http_requests_total{method="GET",route="GET /healthz",status="200"} 1
`
	if err := testutil.GatherAndCompare(serviceMetrics.Registry(), strings.NewReader(expected), "http_requests_total"); err != nil {
		t.Fatalf("request metrics do not match: %v", err)
	}

	// Middleware without metrics (nil) must still pass the request through.
	passthrough := middleware.Chain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }),
		middleware.Metrics(nil),
	)
	recorder := httptest.NewRecorder()
	passthrough.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
}
