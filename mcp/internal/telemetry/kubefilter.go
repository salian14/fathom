package telemetry

import (
	"net/http"
	"regexp"
	"strings"
)

// Discovery endpoints: the API server's root, the group and group-version
// listings, the version probe, and the OpenAPI documents.
//
// These are excluded from tracing for two reasons that reinforce each other.
// They are cache-warming and readiness traffic rather than anything a user
// asked for -- /readyz alone hits /version every ten seconds, forever. And
// client-go's discovery interface has no context-taking method, so these
// requests always carry context.Background() and could only ever arrive as
// root spans, cluttering the trace list without ever joining a real trace.
var discoveryPaths = regexp.MustCompile(`^(?:` +
	`/version|` +
	`/api|` +
	`/apis|` +
	`/api/v1|` +
	`/apis/[^/]+/[^/]+|` +
	`/openapi(?:/.*)?` +
	`)/?$`)

// TraceKubeRequest reports whether a request to the Kubernetes API is worth a
// span. Everything addressing an actual resource is; discovery is not.
func TraceKubeRequest(r *http.Request) bool {
	if r == nil || r.URL == nil {
		return false
	}
	return !discoveryPaths.MatchString(strings.TrimSuffix(r.URL.Path, "/"))
}
