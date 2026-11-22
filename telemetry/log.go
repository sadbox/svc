package telemetry

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// LogHandler wraps h so a record logged with a context that holds a
// sampled span (slog.InfoContext(ctx, ...)) carries trace_id and span_id:
// Grafana links such a log line in Loki to its trace in Tempo. Records
// logged without a context, or outside any span, are unchanged.
func LogHandler(h slog.Handler) slog.Handler {
	return logHandler{h}
}

type logHandler struct {
	slog.Handler
}

func (h logHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() && sc.IsSampled() {
		r = r.Clone()
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, r)
}

func (h logHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return logHandler{h.Handler.WithAttrs(as)}
}

func (h logHandler) WithGroup(name string) slog.Handler {
	return logHandler{h.Handler.WithGroup(name)}
}
