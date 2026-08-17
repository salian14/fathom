package telemetry

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

const sampledTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

func TestTraceHTTPRequest(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})

	tests := []struct {
		name        string
		path        string
		traceparent string
		want        bool
	}{
		// Probes: never, with or without context.
		{"healthz", "/healthz", "", false},
		{"readyz", "/readyz", "", false},
		{"healthz even if traced", "/healthz", sampledTraceparent, false},

		// The readiness check's MCP session: no trace context, because the
		// agent suppresses instrumentation on that path.
		{"unparented mcp post", "/mcp", "", false},
		{"unparented mcp get", "/mcp", "", false},

		// Real work from the agent always propagates.
		{"parented mcp post", "/mcp", sampledTraceparent, true},

		// A malformed header is not trace context.
		{"malformed traceparent", "/mcp", "garbage", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tt.path, nil)
			if tt.traceparent != "" {
				req.Header.Set("traceparent", tt.traceparent)
			}
			if got := TraceHTTPRequest(req); got != tt.want {
				t.Errorf("TraceHTTPRequest(%s, traceparent=%q) = %v, want %v",
					tt.path, tt.traceparent, got, tt.want)
			}
		})
	}
}

func TestTraceHTTPRequestNilSafe(t *testing.T) {
	if TraceHTTPRequest(nil) {
		t.Error("a nil request should not be traced")
	}
}
