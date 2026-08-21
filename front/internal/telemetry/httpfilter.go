package telemetry

import "net/http"

// TraceHTTPRequest reports whether an inbound HTTP request is worth a span.
//
// Only the probes are excluded. kubelet hits /healthz and /readyz every few
// seconds for the life of the pod, and neither says anything a span could add.
//
// This is where fathom-front deliberately diverges from fathom-mcp, and the
// difference is easy to "fix" in the wrong direction later. The MCP server also
// drops requests arriving without trace context, because its only client is the
// agent and an unparented request there is the agent's suppressed readiness
// probe. Here the only client is a browser, which sends no traceparent at all:
// fathom-front is the *root* of these traces, not a middle hop. Applying the
// MCP rule would silence every real request and leave the probes as the only
// thing worth tracing -- exactly backwards.
//
// Static asset requests are traced. They are cheap, they are bounded by the
// page load rather than repeating forever, and a slow first paint is a real
// question someone may ask of the trace.
func TraceHTTPRequest(r *http.Request) bool {
	if r == nil || r.URL == nil {
		return false
	}
	switch r.URL.Path {
	case "/healthz", "/readyz":
		return false
	}
	return true
}
