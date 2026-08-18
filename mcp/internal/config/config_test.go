package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatalf("Load with no env set: %v", err)
	}
	if c.Transport != "http" {
		t.Errorf("Transport = %q, want http", c.Transport)
	}
	if c.RequestTimeout != 30*time.Second {
		t.Errorf("RequestTimeout = %s, want 30s", c.RequestTimeout)
	}
	// Telemetry defaults on: a deploy that forgets to set it should still be
	// observable, and an unreachable collector degrades to no-ops anyway.
	if c.Telemetry != "otlp" {
		t.Errorf("Telemetry = %q, want otlp", c.Telemetry)
	}

	// The default allowlist must not hand a model the cluster's credentials.
	// Read-only is not the same claim as safe to read.
	for _, r := range c.AllowedResources {
		if r == "secrets" {
			t.Fatal("secrets is on the default resource allowlist")
		}
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("FATHOM_MCP_TRANSPORT", "stdio")
	t.Setenv("FATHOM_MCP_ALLOWED_RESOURCES", " pods , deployments.apps ,")
	t.Setenv("FATHOM_MCP_ALLOWED_NAMESPACES", "default,fathom")
	t.Setenv("FATHOM_MCP_REQUEST_TIMEOUT", "5s")
	t.Setenv("FATHOM_MCP_TELEMETRY", "console")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := strings.Join(c.AllowedResources, "|"); got != "pods|deployments.apps" {
		t.Errorf("AllowedResources = %q, want whitespace and empties trimmed", got)
	}
	if !c.AllowsResource("pods") || c.AllowsResource("secrets") {
		t.Error("AllowsResource does not match the configured list")
	}
	if !c.AllowsNamespace("fathom") || c.AllowsNamespace("kube-system") {
		t.Error("AllowsNamespace does not match the configured list")
	}
	if c.RequestTimeout != 5*time.Second {
		t.Errorf("RequestTimeout = %s, want 5s", c.RequestTimeout)
	}
	if c.Telemetry != "console" {
		t.Errorf("Telemetry = %q, want console", c.Telemetry)
	}
}

func TestAllowsNamespaceUnrestricted(t *testing.T) {
	c := &Config{}
	if !c.AllowsNamespace("anything") {
		t.Error("an empty namespace allowlist should mean no namespace restriction")
	}
}

// Bad configuration must stop the process, not degrade it into a server that
// listens with limits nobody chose.
func TestLoadRejectsBadConfig(t *testing.T) {
	cases := map[string]map[string]string{
		"unknown transport":     {"FATHOM_MCP_TRANSPORT": "grpc"},
		"empty allowlist":       {"FATHOM_MCP_ALLOWED_RESOURCES": " , "},
		"subresource allowlist": {"FATHOM_MCP_ALLOWED_RESOURCES": "pods/log"},
		"non-numeric limit":     {"FATHOM_MCP_LIST_LIMIT": "lots"},
		"zero limit":            {"FATHOM_MCP_LIST_LIMIT": "0"},
		"bad duration":          {"FATHOM_MCP_REQUEST_TIMEOUT": "soon"},
		"tail above ceiling":    {"FATHOM_MCP_LOG_TAIL_LINES": "5000", "FATHOM_MCP_LOG_MAX_TAIL_LINES": "2000"},
		"relative mcp path":     {"FATHOM_MCP_MCP_PATH": "mcp"},
		"unknown telemetry":     {"FATHOM_MCP_TELEMETRY": "jaeger"},
	}

	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			for k, v := range env {
				t.Setenv(k, v)
			}
			if _, err := Load(); err == nil {
				t.Error("Load succeeded, want an error")
			}
		})
	}
}

// The empty URL is a working configuration, not a missing one: it is what
// keeps a deployment with no metrics backend from advertising three tools that
// would fail on every call.
func TestMetricsDisabledByDefault(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.PrometheusURL != "" {
		t.Errorf("PrometheusURL = %q, want empty by default", c.PrometheusURL)
	}
	if c.MetricsEnabled() {
		t.Error("MetricsEnabled() is true with no Prometheus configured")
	}
	if c.PrometheusTimeout != 15*time.Second {
		t.Errorf("PrometheusTimeout = %s, want 15s", c.PrometheusTimeout)
	}
	if c.PromQLMaxSeries != 100 || c.MetricsListLimit != 500 {
		t.Errorf("metrics bounds not defaulted: %+v", c)
	}
}

func TestMetricsEnabledByURL(t *testing.T) {
	t.Setenv("FATHOM_MCP_PROMETHEUS_URL", "  http://prometheus.observability.svc:9090  ")
	t.Setenv("FATHOM_MCP_PROMQL_MAX_SERIES", "25")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Trimmed, because a trailing space in a Helm value would otherwise become
	// an unparseable URL at startup.
	if c.PrometheusURL != "http://prometheus.observability.svc:9090" {
		t.Errorf("PrometheusURL = %q, want it trimmed", c.PrometheusURL)
	}
	if !c.MetricsEnabled() {
		t.Error("MetricsEnabled() is false with a URL configured")
	}
	if c.PromQLMaxSeries != 25 {
		t.Errorf("PromQLMaxSeries = %d, want 25", c.PromQLMaxSeries)
	}
}

// A URL that cannot work must fail the pod at startup, where the error names
// the variable, rather than at the first tool call.
func TestLoadRejectsBadPrometheusConfig(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"no scheme", map[string]string{"PROMETHEUS_URL": "prometheus:9090"}, "PROMETHEUS_URL"},
		{"wrong scheme", map[string]string{"PROMETHEUS_URL": "ftp://p:9090"}, "PROMETHEUS_URL"},
		{"no host", map[string]string{"PROMETHEUS_URL": "http://"}, "PROMETHEUS_URL"},
		{"zero timeout", map[string]string{"PROMETHEUS_TIMEOUT": "0s"}, "PROMETHEUS_TIMEOUT"},
		{"zero series cap", map[string]string{"PROMQL_MAX_SERIES": "0"}, "PROMQL_MAX_SERIES"},
		{"negative query cap", map[string]string{"PROMQL_MAX_QUERY_LENGTH": "-1"}, "PROMQL_MAX_QUERY_LENGTH"},
		{"zero label values", map[string]string{"METRICS_LABEL_VALUES": "0"}, "METRICS_LABEL_VALUES"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv("FATHOM_MCP_"+k, v)
			}
			_, err := Load()
			if err == nil {
				t.Fatalf("Load accepted %v", tt.env)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not name %s", err, tt.want)
			}
		})
	}
}
