package handler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/example/wallet-service/internal/http/handler"
)

// TestRouterRegistersWithoutConflict ensures every route pattern can be
// registered on a single http.ServeMux. ServeMux panics when a pattern conflicts,
// so this test is the safety net whenever a route is added (e.g. combining
// `METHOD /path` with a `/path` fallback).
func TestRouterRegistersWithoutConflict(t *testing.T) {
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("route registration panicked: %v", recovered)
		}
	}()

	router := handler.NewRouter(handler.Dependencies{
		Wallets:   &handler.WalletHandler{},
		Movements: &handler.MovementHandler{},
		Health:    &handler.HealthHandler{},
	})
	if router == nil {
		t.Fatal("router nil")
	}
}

func TestRouterUnknownPathReturnsProblemDetails(t *testing.T) {
	router := handler.NewRouter(handler.Dependencies{
		Wallets:   &handler.WalletHandler{},
		Movements: &handler.MovementHandler{},
		Health:    &handler.HealthHandler{},
	})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/does-not-exist", nil))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "application/json; charset=utf-8" {
		t.Errorf("content-type = %q", contentType)
	}

	var problem map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &problem); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if problem["code"] != "not_found" {
		t.Errorf("code = %v, want not_found", problem["code"])
	}
	for _, field := range []string{"type", "title", "status", "detail", "instance"} {
		if _, ok := problem[field]; !ok {
			t.Errorf("RFC 7807 field %q is missing from the response", field)
		}
	}
}

func TestRouterMethodNotAllowedReturnsProblemDetails(t *testing.T) {
	router := handler.NewRouter(handler.Dependencies{
		Wallets:   &handler.WalletHandler{},
		Movements: &handler.MovementHandler{},
		Health:    &handler.HealthHandler{},
	})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/wallets", nil))

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", recorder.Code)
	}
	if allow := recorder.Header().Get("Allow"); allow != http.MethodPost {
		t.Errorf("Allow header = %q, want POST", allow)
	}

	var problem map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &problem); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if problem["code"] != "method_not_allowed" {
		t.Errorf("code = %v, want method_not_allowed", problem["code"])
	}
}
