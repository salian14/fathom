package tools

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/spellbound/fathom/mcp/internal/config"
)

func testServer(cfg *config.Config) *Server {
	// Both clients are nil: everything exercised here decides whether a
	// request is permissible, or which tools are registered, before any client
	// call would happen.
	return New(cfg, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func restMapping(resource string, namespaced bool) *meta.RESTMapping {
	scope := meta.RESTScopeRoot
	if namespaced {
		scope = meta.RESTScopeNamespace
	}
	return &meta.RESTMapping{
		Resource: schema.GroupVersionResource{Version: "v1", Resource: resource},
		Scope:    scope,
	}
}

// The tool list is the entire prompt surface the model sees. If a tool goes
// missing or an argument is renamed, the agent silently loses a capability, so
// pin the contract.
//
// Both halves are pinned, because the metrics tools are conditional: the
// without-Prometheus case is what protects a deployment that has no metrics
// backend from being shown three tools that would fail on every call, and it
// is the case that regresses silently if the gate is ever removed.
func TestRegisteredToolContract(t *testing.T) {
	kubeTools := map[string][]string{
		"k8s_get":      {"resource", "namespace", "name", "allNamespaces", "labelSelector"},
		"k8s_describe": {"resource", "name", "namespace"},
		"k8s_log":      {"name", "namespace", "container", "tailLines", "previous"},
	}
	metricsTools := map[string][]string{
		"list_metrics":    {"contains", "match", "limit"},
		"describe_metric": {"metric", "match"},
		"run_promql":      {"query", "time"},
	}

	t.Run("without prometheus", func(t *testing.T) {
		assertToolContract(t, &config.Config{}, kubeTools)
	})

	t.Run("with prometheus", func(t *testing.T) {
		want := map[string][]string{}
		for name, props := range kubeTools {
			want[name] = props
		}
		for name, props := range metricsTools {
			want[name] = props
		}
		assertToolContract(t, &config.Config{PrometheusURL: "http://prometheus.test:9090"}, want)
	})
}

func assertToolContract(t *testing.T, cfg *config.Config, want map[string][]string) {
	t.Helper()
	ctx := context.Background()

	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "v0"}, nil)
	testServer(cfg).Register(srv)

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil).
		Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	found := map[string]*mcp.Tool{}
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		found[tool.Name] = tool
	}

	if len(found) != len(want) {
		t.Fatalf("registered %d tools (%v), want exactly %d", len(found), keys(found), len(want))
	}

	for name, properties := range want {
		tool, ok := found[name]
		if !ok {
			t.Errorf("tool %q is not registered", name)
			continue
		}
		if tool.Description == "" {
			t.Errorf("tool %q has no description; the model chooses tools from these", name)
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("tool %q is not annotated read-only", name)
		}

		schema := marshalSchema(t, tool.InputSchema)
		for _, p := range properties {
			if !strings.Contains(schema, `"`+p+`"`) {
				t.Errorf("tool %q input schema is missing property %q", name, p)
			}
		}
	}
}

func marshalSchema(t *testing.T, s any) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshalling input schema: %v", err)
	}
	return string(b)
}

func keys(m map[string]*mcp.Tool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestNamespaceForDefaulting(t *testing.T) {
	s := testServer(&config.Config{DefaultNamespace: "fathom"})

	got, err := s.namespaceFor(restMapping("pods", true), "", false)
	if err != nil {
		t.Fatalf("namespaceFor: %v", err)
	}
	if got != "fathom" {
		t.Errorf("namespaceFor(\"\") = %q, want the configured default", got)
	}

	// An empty namespace on the wire is what makes a list span namespaces.
	if got, err := s.namespaceFor(restMapping("pods", true), "", true); err != nil || got != "" {
		t.Errorf("namespaceFor(allNamespaces) = %q, %v; want \"\", nil", got, err)
	}
}

func TestNamespaceForClusterScoped(t *testing.T) {
	s := testServer(&config.Config{DefaultNamespace: "default"})

	if got, err := s.namespaceFor(restMapping("nodes", false), "", false); err != nil || got != "" {
		t.Errorf("cluster-scoped namespaceFor = %q, %v; want \"\", nil", got, err)
	}
	// Passing a namespace for nodes is a model mistake worth naming, not
	// quietly ignoring: the answer it expects and the answer it gets differ.
	if _, err := s.namespaceFor(restMapping("nodes", false), "default", false); err == nil {
		t.Error("a namespace on a cluster-scoped resource was accepted")
	}
}

func TestNamespaceForAllowlist(t *testing.T) {
	s := testServer(&config.Config{
		DefaultNamespace:  "default",
		AllowedNamespaces: []string{"default", "fathom"},
	})

	if _, err := s.namespaceFor(restMapping("pods", true), "fathom", false); err != nil {
		t.Errorf("permitted namespace rejected: %v", err)
	}
	if _, err := s.namespaceFor(restMapping("pods", true), "kube-system", false); err == nil {
		t.Error("namespace outside the allowlist was accepted")
	}
	// allNamespaces would read straight past the allowlist, so it cannot be
	// silently honoured while one is configured.
	if _, err := s.namespaceFor(restMapping("pods", true), "", true); err == nil {
		t.Error("allNamespaces was accepted despite a namespace allowlist")
	}
}
