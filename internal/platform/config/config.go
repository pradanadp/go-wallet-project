// Package config loads configuration from environment variables (12-factor app).
//
// Parsing is done manually without a config library (Viper/kong) because the needs
// are simple and we want to fail fast: every invalid value is collected and
// reported at once during startup, not when the first request arrives.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/example/wallet-service/internal/domain"
)

// Environment is the application environment name.
type Environment string

const (
	EnvDevelopment Environment = "development"
	EnvStaging     Environment = "staging"
	EnvProduction  Environment = "production"
)

// Config is the whole service configuration.
type Config struct {
	Env  Environment
	Addr string

	// Database
	DatabaseURL        string
	DBMaxConns         int32
	DBMinConns         int32
	DBConnMaxLifetime  time.Duration
	DBConnMaxIdleTime  time.Duration
	DBConnectTimeout   time.Duration
	DBStatementTimeout time.Duration
	MigrateOnStart     bool

	// HTTP
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	RequestTimeout    time.Duration
	ShutdownTimeout   time.Duration
	MaxBodyBytes      int64

	// Security
	APIKey         string
	RateLimitRPS   float64
	RateLimitBurst int

	// Domain
	SystemWalletID         domain.ID
	IdempotencyTTL         time.Duration
	IdempotencyCleanupFreq time.Duration

	// Logging
	LogLevel  string
	LogFormat string
}

// IsProduction marks environments that need strict behaviour (JSON logs, etc.).
func (c *Config) IsProduction() bool { return c.Env == EnvProduction }

// Load reads the environment and validates it.
func Load() (*Config, error) {
	l := &loader{}

	cfg := &Config{
		Env:                    Environment(l.str("APP_ENV", string(EnvDevelopment))),
		Addr:                   l.str("HTTP_ADDR", ":8080"),
		DatabaseURL:            l.str("DATABASE_URL", ""),
		DBMaxConns:             l.int32("DB_MAX_CONNS", 10),
		DBMinConns:             l.int32("DB_MIN_CONNS", 2),
		DBConnMaxLifetime:      l.duration("DB_CONN_MAX_LIFETIME", 30*time.Minute),
		DBConnMaxIdleTime:      l.duration("DB_CONN_MAX_IDLE_TIME", 5*time.Minute),
		DBConnectTimeout:       l.duration("DB_CONNECT_TIMEOUT", 5*time.Second),
		DBStatementTimeout:     l.duration("DB_STATEMENT_TIMEOUT", 5*time.Second),
		MigrateOnStart:         l.boolean("MIGRATE_ON_START", false),
		ReadHeaderTimeout:      l.duration("HTTP_READ_HEADER_TIMEOUT", 5*time.Second),
		ReadTimeout:            l.duration("HTTP_READ_TIMEOUT", 15*time.Second),
		WriteTimeout:           l.duration("HTTP_WRITE_TIMEOUT", 20*time.Second),
		IdleTimeout:            l.duration("HTTP_IDLE_TIMEOUT", 60*time.Second),
		RequestTimeout:         l.duration("REQUEST_TIMEOUT", 10*time.Second),
		ShutdownTimeout:        l.duration("SHUTDOWN_TIMEOUT", 15*time.Second),
		MaxBodyBytes:           l.int64("MAX_BODY_BYTES", 1<<20),
		APIKey:                 l.str("API_KEY", ""),
		RateLimitRPS:           l.float64("RATE_LIMIT_RPS", 50),
		RateLimitBurst:         l.integer("RATE_LIMIT_BURST", 100),
		IdempotencyTTL:         l.duration("IDEMPOTENCY_TTL", 24*time.Hour),
		IdempotencyCleanupFreq: l.duration("IDEMPOTENCY_CLEANUP_INTERVAL", 15*time.Minute),
		LogLevel:               strings.ToLower(l.str("LOG_LEVEL", "info")),
		LogFormat:              strings.ToLower(l.str("LOG_FORMAT", "")),
	}

	systemWalletID := l.str("SYSTEM_WALLET_ID", "00000000-0000-0000-0000-000000000001")
	if id, err := domain.ParseID("SYSTEM_WALLET_ID", systemWalletID); err != nil {
		l.failf("SYSTEM_WALLET_ID: %v", err)
	} else {
		cfg.SystemWalletID = id
	}

	switch cfg.Env {
	case EnvDevelopment, EnvStaging, EnvProduction:
	default:
		l.failf("APP_ENV %q is not recognized (choose development, staging, or production)", cfg.Env)
	}
	if cfg.DatabaseURL == "" {
		l.failf("DATABASE_URL is required")
	}
	if cfg.DBMinConns > cfg.DBMaxConns {
		l.failf("DB_MIN_CONNS (%d) must not be greater than DB_MAX_CONNS (%d)", cfg.DBMinConns, cfg.DBMaxConns)
	}
	if cfg.RateLimitRPS < 0 || cfg.RateLimitBurst < 0 {
		l.failf("RATE_LIMIT_RPS and RATE_LIMIT_BURST must not be negative")
	}
	if cfg.RequestTimeout <= 0 {
		l.failf("REQUEST_TIMEOUT must be greater than 0")
	}
	if cfg.IsProduction() && cfg.APIKey == "" {
		l.failf("API_KEY is required in the production environment")
	}

	if err := l.err(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// loader collects every configuration problem so the user fixes them all at
// once instead of one at a time.
type loader struct {
	problems []string
}

func (l *loader) failf(format string, args ...any) {
	l.problems = append(l.problems, fmt.Sprintf(format, args...))
}

func (l *loader) err() error {
	if len(l.problems) == 0 {
		return nil
	}
	msgs := make([]error, 0, len(l.problems))
	for _, p := range l.problems {
		msgs = append(msgs, errors.New(p))
	}
	return fmt.Errorf("invalid configuration:\n  - %w", errors.Join(msgs...))
}

func (l *loader) str(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return strings.TrimSpace(v)
	}
	return def
}

func (l *loader) integer(key string, def int) int {
	raw := l.str(key, "")
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		l.failf("%s=%q is not a valid number", key, raw)
		return def
	}
	return v
}

func (l *loader) int32(key string, def int32) int32 {
	return int32(l.integer(key, int(def)))
}

func (l *loader) int64(key string, def int64) int64 {
	raw := l.str(key, "")
	if raw == "" {
		return def
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		l.failf("%s=%q is not a valid number", key, raw)
		return def
	}
	return v
}

func (l *loader) float64(key string, def float64) float64 {
	raw := l.str(key, "")
	if raw == "" {
		return def
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		l.failf("%s=%q is not a valid decimal number", key, raw)
		return def
	}
	return v
}

func (l *loader) boolean(key string, def bool) bool {
	raw := l.str(key, "")
	if raw == "" {
		return def
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		l.failf("%s=%q is not a valid boolean (true/false)", key, raw)
		return def
	}
	return v
}

func (l *loader) duration(key string, def time.Duration) time.Duration {
	raw := l.str(key, "")
	if raw == "" {
		return def
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		l.failf("%s=%q is not a valid duration (e.g. 5s, 1m30s)", key, raw)
		return def
	}
	return v
}
