package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
)

// Config controls the runtime logging behavior.
type Config struct {
	Level   string            `json:"level"`
	Format  string            `json:"format"`
	Modules map[string]string `json:"modules"`
}

type contextKey struct{}

// Manager builds module-specific loggers with configurable levels.
type Manager struct {
	defaultLogger *slog.Logger
	defaultLevel  slog.Level
	format        string
	moduleLevels  map[string]slog.Level
	moduleLoggers map[string]*slog.Logger
	mu            sync.RWMutex
}

// NewManager creates a Manager with the supplied configuration.
func NewManager(cfg Config) (*Manager, error) {
	level, err := parseLevel(cfg.Level)
	if err != nil {
		return nil, fmt.Errorf("invalid default log level: %w", err)
	}

	format := strings.ToLower(strings.TrimSpace(cfg.Format))
	if format == "" {
		format = "text"
	}

	handler, err := buildHandler(os.Stdout, format, level)
	if err != nil {
		return nil, err
	}

	moduleLevels := make(map[string]slog.Level)
	for module, levelStr := range cfg.Modules {
		lvl, err := parseLevel(levelStr)
		if err != nil {
			return nil, fmt.Errorf("invalid log level for module %s: %w", module, err)
		}
		moduleLevels[module] = lvl
	}

	return &Manager{
		defaultLogger: slog.New(handler),
		defaultLevel:  level,
		format:        format,
		moduleLevels:  moduleLevels,
		moduleLoggers: make(map[string]*slog.Logger),
	}, nil
}

// Default returns the default logger instance.
func (m *Manager) Default() *slog.Logger {
	return m.defaultLogger
}

// Logger returns a module-specific logger respecting configured log levels.
func (m *Manager) Logger(module string) *slog.Logger {
	module = strings.TrimSpace(module)
	if module == "" {
		return m.defaultLogger
	}

	m.mu.RLock()
	if logger, ok := m.moduleLoggers[module]; ok {
		m.mu.RUnlock()
		return logger
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()

	if logger, ok := m.moduleLoggers[module]; ok {
		return logger
	}

	level := m.defaultLevel
	if lvl, ok := m.moduleLevels[module]; ok {
		level = lvl
	}

	var logger *slog.Logger
	if level == m.defaultLevel {
		logger = m.defaultLogger.With(slog.String("module", module))
	} else {
		handler, err := buildHandler(os.Stdout, m.format, level)
		if err != nil {
			// fall back to default logger if handler creation fails
			logger = m.defaultLogger.With(slog.String("module", module))
		} else {
			logger = slog.New(handler).With(slog.String("module", module))
		}
	}

	m.moduleLoggers[module] = logger
	return logger
}

// ContextWithLogger attaches the module-specific logger to the context.
func (m *Manager) ContextWithLogger(ctx context.Context, module string) context.Context {
	return context.WithValue(ctx, contextKey{}, m.Logger(module))
}

// FromContext retrieves a logger from the context, or the default logger if absent.
func (m *Manager) FromContext(ctx context.Context) *slog.Logger {
	if ctx == nil {
		return m.defaultLogger
	}
	if logger, ok := ctx.Value(contextKey{}).(*slog.Logger); ok && logger != nil {
		return logger
	}
	return m.defaultLogger
}

func parseLevel(level string) (slog.Level, error) {
	if level == "" {
		return slog.LevelInfo, nil
	}
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("unknown level %q", level)
	}
}

func buildHandler(w io.Writer, format string, level slog.Level) (slog.Handler, error) {
	opts := &slog.HandlerOptions{Level: level}
	switch format {
	case "json":
		return slog.NewJSONHandler(w, opts), nil
	case "text":
		fallthrough
	case "":
		return slog.NewTextHandler(w, opts), nil
	default:
		return nil, fmt.Errorf("unsupported log format %q", format)
	}
}
