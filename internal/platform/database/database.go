// Package database sets up the PostgreSQL connection pool (pgxpool).
package database

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/example/wallet-service/internal/platform/config"
)

// NewPool builds a configured pgxpool and verifies the connection on the first
// attempt, so credential or network problems surface at startup (fail fast)
// instead of on the first user request.
func NewPool(ctx context.Context, cfg *config.Config) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}

	poolCfg.MaxConns = cfg.DBMaxConns
	poolCfg.MinConns = cfg.DBMinConns
	poolCfg.MaxConnLifetime = cfg.DBConnMaxLifetime
	poolCfg.MaxConnIdleTime = cfg.DBConnMaxIdleTime
	poolCfg.HealthCheckPeriod = 30 * time.Second
	poolCfg.ConnConfig.ConnectTimeout = cfg.DBConnectTimeout

	// RuntimeParams are sent during the connection handshake:
	//   - application_name: makes connections easy to spot in pg_stat_activity;
	//   - statement_timeout: a database-level safety net so an out-of-control query
	//     (e.g. waiting too long on a lock) is cancelled by the server instead of
	//     hanging until the HTTP timeout.
	poolCfg.ConnConfig.RuntimeParams["application_name"] = "wallet-service"
	poolCfg.ConnConfig.RuntimeParams["statement_timeout"] = strconv.FormatInt(cfg.DBStatementTimeout.Milliseconds(), 10)
	poolCfg.ConnConfig.RuntimeParams["timezone"] = "UTC"

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("creating the connection pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, cfg.DBConnectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connecting to PostgreSQL failed: %w", err)
	}

	slog.Info("PostgreSQL connection ready",
		slog.Int("max_conns", int(poolCfg.MaxConns)),
		slog.Int("min_conns", int(poolCfg.MinConns)),
		slog.String("statement_timeout", cfg.DBStatementTimeout.String()),
	)
	return pool, nil
}
