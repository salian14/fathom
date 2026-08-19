package telemetry

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTraceHTTPRequest(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		headers map[string]string
		want    bool
	}{
		{"liveness probe", "/healthz", nil, false},
		{"readiness probe", "/readyz", nil, false},
		{"chat without trace context", "/api/chat", nil, true},
		{"page without trace context", "/", nil, true},
		{"asset without trace context", "/app.js", nil, true},
		{
			// The rule fathom-mcp applies -- drop what carries no trace context
			// -- must not creep in here. A browser never sends traceparent, so
			// presence or absence of the header changes nothing.
			name:    "chat with trace context",
			path:    "/api/chat",
			headers: map[string]string{"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tt.path, nil)
			for k, v := range tt.headers {
				r.Header.Set(k, v)
			}
			if got := TraceHTTPRequest(r); got != tt.want {
				t.Errorf("TraceHTTPRequest(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestTraceHTTPRequestNil(t *testing.T) {
	if TraceHTTPRequest(nil) {
		t.Error("TraceHTTPRequest(nil) = true, want false")
	}
	if TraceHTTPRequest(&http.Request{}) {
		t.Error("TraceHTTPRequest(request with nil URL) = true, want false")
	}
}
