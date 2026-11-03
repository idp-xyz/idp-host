// Package observability provides structured logging and OpenTelemetry tracing (tracing.go). The
// gateway continues a valid incoming W3C trace context or starts a new trace, so audit records, logs
// and upstream calls correlate on one trace id. Trace context is a correlator, never an
// authorization input.
package observability

import (
	"io"
	"log/slog"
	"strings"
)

// NewLogger returns a JSON logger at the given level ("debug", "info", "warn", "error").
func NewLogger(w io.Writer, level string) *slog.Logger {
	var l slog.Level
	_ = l.UnmarshalText([]byte(strings.ToUpper(level)))
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: l}))
}
