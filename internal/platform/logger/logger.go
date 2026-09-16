// Package logger sets up log/slog according to the environment.
package logger

import (
	"log/slog"
	"os"

	"github.com/example/wallet-service/internal/platform/config"
)

// New builds a structured logger:
//   - JSON is used in production (easy for log aggregators to parse) or when
//     LOG_FORMAT=json is forced;
//   - otherwise text, which is comfortable to read in a terminal during development.
//
// log/slog is chosen over Zap/Zerolog because it is already part of the standard
// library (zero dependencies), ships a JSON handler, and supports
// context-aware logging through Logger.With/WithContext.
func New(env config.Environment, level, format string) *slog.Logger {
	production := env == config.EnvProduction
	handlerOpts := &slog.HandlerOptions{
		Level: parseLevel(level),
		// The source line is only useful during development; in production it only adds
		// noise and runtime cost.
		AddSource: !production,
	}

	var handler slog.Handler
	if format == "json" || (format == "" && env != config.EnvDevelopment) {
		handler = slog.NewJSONHandler(os.Stdout, handlerOpts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, handlerOpts)
	}

	logger := slog.New(handler).With(
		slog.String("service", "wallet-service"),
		slog.String("env", string(env)),
	)

	// Make it the default so code using slog.Default() (including http.Server when
	// not configured) follows the same format.
	slog.SetDefault(logger)
	return logger
}

func parseLevel(raw string) slog.Level {
	switch raw {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
