package telemetry

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// SlogHandler adds trace_id and span_id to any record logged with a context
// carrying a span, so a line found in `kubectl logs` can be pivoted to its
// trace and back again.
//
// Note that only the Context-taking methods (InfoContext, WarnContext, ...)
// can carry ids: plain slog.Info passes context.Background(). Logging inside a
// request path is therefore worth the extra argument; logging at startup is
// not, and correctly gets no ids.
type SlogHandler struct {
	slog.Handler
}

// NewSlogHandler wraps h so records gain trace correlation fields.
func NewSlogHandler(h slog.Handler) slog.Handler {
	return &SlogHandler{Handler: h}
}

func (h *SlogHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, r)
}

func (h *SlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &SlogHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *SlogHandler) WithGroup(name string) slog.Handler {
	return &SlogHandler{Handler: h.Handler.WithGroup(name)}
}
