// Command api is the entrypoint of the Wallet & Ledger Service.
//
// Every dependency is assembled here explicitly (manual constructor injection) so
// the dependency graph is readable from a single file and there is no magic such
// as reflection or a DI container.
//
// Usage:
//
//	api                     run the HTTP server
//	api migrate up|down|version   run migrations and exit
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/example/wallet-service/internal/http/handler"
	"github.com/example/wallet-service/internal/http/middleware"
	"github.com/example/wallet-service/internal/platform/config"
	"github.com/example/wallet-service/internal/platform/database"
	"github.com/example/wallet-service/internal/platform/logger"
	"github.com/example/wallet-service/internal/platform/metrics"
	"github.com/example/wallet-service/internal/platform/migrate"
	"github.com/example/wallet-service/internal/repository/postgres"
	"github.com/example/wallet-service/internal/usecase"
	"github.com/example/wallet-service/pkg/uuid"
)

// version is set at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

// idempotencyPurgeTimeout caps the duration of one housekeeping cycle.
const idempotencyPurgeTimeout = 30 * time.Second

// healthcheckTimeout caps the internal health check (used by docker).
const healthcheckTimeout = 3 * time.Second

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("service stopped with an error", slog.Any("error", err))
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "migrate":
			return runMigrate(args[1:])
		case "healthcheck":
			return runHealthcheck()
		default:
			return fmt.Errorf("unknown command %q (available: migrate up|down|version, healthcheck)", args[0])
		}
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := logger.New(cfg.Env, cfg.LogLevel, cfg.LogFormat)

	// signal.NotifyContext: SIGINT/SIGTERM cancels ctx so that every task honouring
	// ctx stops cleanly (graceful shutdown).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.MigrateOnStart {
		log.Info("running migrations at startup")
		if err := migrate.Run(ctx, migrate.CommandUp, cfg.DatabaseURL, log); err != nil {
			return fmt.Errorf("startup migration failed: %w", err)
		}
	}

	pool, err := database.NewPool(ctx, cfg)
	if err != nil {
		return err
	}
	// The defer order matters: MarkShuttingDown must run before the pool is closed so
	// the pool metric gauges never read an invalid pool.
	defer pool.Close()
	serviceMetrics := metrics.New(pool)
	defer serviceMetrics.MarkShuttingDown()

	// --- Dependency wiring (inside out) ---
	unitOfWork := postgres.NewUnitOfWork(pool, log)

	walletUseCase := usecase.NewWalletUseCase(unitOfWork, time.Now, uuid.V4, cfg.IdempotencyTTL)
	movementUseCase := usecase.NewMovementUseCase(unitOfWork, time.Now, uuid.V4, cfg.SystemWalletID, cfg.IdempotencyTTL)

	walletHandler := handler.NewWalletHandler(walletUseCase, serviceMetrics, cfg.MaxBodyBytes, log)
	movementHandler := handler.NewMovementHandler(movementUseCase, serviceMetrics, cfg.MaxBodyBytes, log)
	healthHandler := handler.NewHealthHandler(pool, version)

	router := handler.NewRouter(
		handler.Dependencies{
			Wallets:   walletHandler,
			Movements: movementHandler,
			Health:    healthHandler,
			Metrics:   serviceMetrics.Handler(),
		},
		// The middleware order defines the request execution order (outermost first).
		middleware.RequestID(),
		middleware.Logger(log),
		middleware.Metrics(serviceMetrics),
		// Recover is installed inside Logger & Metrics so a panic is still recorded as
		// status 500 in the log and metrics of that request.
		middleware.Recover(),
		middleware.Timeout(cfg.RequestTimeout),
		middleware.RateLimit(cfg.RateLimitRPS, cfg.RateLimitBurst),
		middleware.Auth(cfg.APIKey),
	)

	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           router,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	go runIdempotencyJanitor(ctx, postgres.NewMaintenance(pool), cfg.IdempotencyCleanupFreq, log)

	serverErr := make(chan error, 1)
	go func() {
		log.Info("HTTP server listening",
			slog.String("addr", cfg.Addr),
			slog.Bool("auth_enabled", cfg.APIKey != ""),
			slog.String("version", version),
		)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		return fmt.Errorf("HTTP server failed: %w", err)
	case <-ctx.Done():
		log.Info("stop signal received, shutting the server down gracefully")
	}

	// A fresh context (not the already cancelled ctx) so in-flight requests still get
	// time to finish.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown failed: %w", err)
	}
	log.Info("server stopped gracefully")
	return nil
}

// runMigrate runs migrations without starting the HTTP server.
// DATABASE_URL is read straight from the environment so this command does not
// require other runtime configuration (such as API_KEY) that is irrelevant here.
func runMigrate(args []string) error {
	command := migrate.CommandUp
	if len(args) > 0 {
		command = migrate.Command(args[0])
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required to run migrations")
	}

	log := logger.New(config.EnvDevelopment, "info", "text")
	return migrate.Run(context.Background(), command, dsn, log)
}

// runHealthcheck is used as the docker HEALTHCHECK.
//
// The binary provides its own subcommand because the distroless runtime image has
// no shell or curl to run a health check.
func runHealthcheck() error {
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("HTTP_ADDR %q is invalid: %w", addr, err)
	}
	// ":8080" or "0.0.0.0:8080" means all interfaces; inside a container the
	// loopback address still works for a health check.
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}

	url := fmt.Sprintf("http://%s/healthz", net.JoinHostPort(host, port))
	client := &http.Client{Timeout: healthcheckTimeout}

	response, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("health check to %s failed: %w", url, err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("health check to %s returned status %d", url, response.StatusCode)
	}
	return nil
}

// runIdempotencyJanitor purges idempotency keys whose TTL has passed.
//
// Expired keys only need cleanup so the table does not grow forever; the
// "at most once" guarantee holds as long as the row still exists.
func runIdempotencyJanitor(ctx context.Context, maintenance *postgres.Maintenance, interval time.Duration, log *slog.Logger) {
	if interval <= 0 {
		log.Info("the idempotency janitor is disabled")
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("the idempotency janitor stopped")
			return
		case <-ticker.C:
			purgeCtx, cancel := context.WithTimeout(ctx, idempotencyPurgeTimeout)
			deleted, err := maintenance.PurgeExpiredIdempotencyKeys(purgeCtx)
			cancel()

			if err != nil {
				log.Warn("failed to purge expired idempotency keys", slog.Any("error", err))
				continue
			}
			if deleted > 0 {
				log.Info("expired idempotency keys purged", slog.Int64("deleted", deleted))
			}
		}
	}
}
