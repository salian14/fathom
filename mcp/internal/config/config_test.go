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
