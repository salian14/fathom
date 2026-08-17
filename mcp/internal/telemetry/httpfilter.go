package telemetry

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// TraceHTTPRequest reports whether an inbound HTTP request is worth a span.
//
// Two exclusions, for the same underlying reason -- neither says anything a
// span could add, and both repeat for the life of the pod:
//
//   - The probes themselves. kubelet hits them every 10-20s.
//   - Anything arriving without trace context. The only client is the agent,
//     and it propagates on every real call; the one path where it deliberately
//     does not is its readiness check, which opens a full MCP session and would
//     otherwise cost eight spans every time it runs.
//
// The second rule is why the MCP middleware still traces tools/call regardless:
// if propagation ever breaks, real work must not go quiet along with the noise.
func TraceHTTPRequest(r *http.Request) bool {
	if r == nil || r.URL == nil {
		return false
	}
	switch r.URL.Path {
	case "/healthz", "/readyz":
		return false
	}
	return HasTraceContext(r.Header)
}

// HasTraceContext reports whether h carries usable W3C trace context. It
// extracts rather than checking for the header, so a malformed traceparent
// counts as absent.
func HasTraceContext(h http.Header) bool {
	ctx := otel.GetTextMapPropagator().Extract(context.Background(), propagation.HeaderCarrier(h))
	return trace.SpanContextFromContext(ctx).IsValid()
}
