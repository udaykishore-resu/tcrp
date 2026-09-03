package obs

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
)

// NewLogger builds a structured JSON logger. Every log line carries the trace
// and span id when one is in context, so a log search and a trace search return
// the same set of records.
func NewLogger(w io.Writer, level string) *slog.Logger {
	if w == nil {
		w = os.Stdout
	}
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(&traceHandler{inner: slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lv})})
}

type traceHandler struct{ inner slog.Handler }

func (h *traceHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sp := SpanFromContext(ctx); sp != nil {
		sc := sp.Context()
		r.AddAttrs(slog.String("trace_id", sc.TraceID), slog.String("span_id", sc.SpanID))
	}
	return h.inner.Handle(ctx, r)
}

func (h *traceHandler) WithAttrs(a []slog.Attr) slog.Handler {
	return &traceHandler{inner: h.inner.WithAttrs(a)}
}

func (h *traceHandler) WithGroup(n string) slog.Handler {
	return &traceHandler{inner: h.inner.WithGroup(n)}
}

// Discard is a logger that drops everything, for tests.
func Discard() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }
