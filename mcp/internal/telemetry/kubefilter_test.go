package telemetry

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTraceKubeRequest(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		// Discovery and probes: excluded. These carry context.Background()
		// because client-go's discovery interface takes no context, so they
		// could only ever be root spans.
		{"/version", false},
		{"/version/", false},
		{"/api", false},
		{"/apis", false},
		{"/api/v1", false},
		{"/apis/apps/v1", false},
		{"/apis/networking.k8s.io/v1", false},
		{"/openapi/v2", false},
		{"/openapi/v3/apis/apps/v1", false},

		// Real reads: traced.
		{"/api/v1/namespaces/default/pods", true},
		{"/api/v1/namespaces/kube-system/pods/coredns-abc/log", true},
		{"/api/v1/nodes", true},
		{"/apis/apps/v1/namespaces/fathom/deployments", true},
		{"/apis/batch/v1/namespaces/default/jobs/backup", true},
		{"/api/v1/namespaces", true},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			if got := TraceKubeRequest(req); got != tt.want {
				t.Errorf("TraceKubeRequest(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestTraceKubeRequestNilSafe(t *testing.T) {
	if TraceKubeRequest(nil) {
		t.Error("a nil request should not be traced")
	}
}
