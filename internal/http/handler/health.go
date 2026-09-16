package handler

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/example/wallet-service/pkg/apierror"
	"github.com/example/wallet-service/pkg/response"
)

// readinessTimeout caps how long /readyz checks its dependencies. A probe must
// always answer quickly so the orchestrator never wrongly concludes the service is dead.
const readinessTimeout = 2 * time.Second

// HealthHandler serves the liveness and readiness probes.
type HealthHandler struct {
	db      Pinger
	version string
}

func NewHealthHandler(db Pinger, version string) *HealthHandler {
	return &HealthHandler{db: db, version: version}
}

type healthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version,omitempty"`
	Uptime  string `json:"uptime,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

var startedAt = time.Now()

// Live handles GET /healthz (liveness).
//
// It deliberately does not touch the database: liveness only answers "this process is alive".
// If the database were checked here, a database outage would make the
// orchestrator restart every pod for no reason.
func (h *HealthHandler) Live(w http.ResponseWriter, r *http.Request) {
	response.JSON(w, http.StatusOK, healthResponse{
		Status:  "ok",
		Version: h.version,
		Uptime:  time.Since(startedAt).Round(time.Second).String(),
	})
}

// Ready handles GET /readyz (readiness).
//
// Here the dependency is checked: a failed readiness means the pod must not take
// new traffic, but it does not need a restart.
func (h *HealthHandler) Ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
	defer cancel()

	if err := h.db.Ping(ctx); err != nil {
		slog.WarnContext(ctx, "readiness failed: database is unreachable", slog.Any("error", err))
		response.Error(w, r, apierror.Unavailable("database is unreachable"))
		return
	}

	response.JSON(w, http.StatusOK, healthResponse{Status: "ok", Version: h.version})
}
