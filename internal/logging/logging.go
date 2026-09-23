// Package logging provides Hive's structured logger and the stable field
// names that log consumers may rely on.
package logging

import (
	"io"
	"log/slog"
	"strings"
)

// Stable structured log field names.
const (
	FieldComponent = "component"
	FieldRole      = "role"
	FieldNodeID    = "node_id"
	FieldPluginID  = "plugin_id"
	FieldSessionID = "session_id"
	FieldRunID     = "run_id"
	FieldCommandID = "command_id"
	FieldEventID   = "event_id"
	FieldMethod    = "method"
	FieldState     = "state"
	FieldError     = "error"
)

// New returns a logger writing to w with the given level and format.
//
// Supported formats are "text" and "json". Supported levels are "debug",
// "info", "warn", and "error"; anything else is treated as "info".
func New(w io.Writer, level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: ParseLevel(level)}
	var h slog.Handler
	if strings.EqualFold(strings.TrimSpace(format), "json") {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	return slog.New(h)
}

// ParseLevel converts a configured level name to a slog level.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
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
