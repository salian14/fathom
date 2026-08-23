package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/spellbound/fathom/mcp/internal/config"
	"github.com/spellbound/fathom/mcp/internal/tools"
)

// contractSchemaVersion is the version of the document this file emits, not of
// anything it describes. Bump it when the shape below changes so a reader that
// caches contracts can tell a reshape from a real interface change.
const contractSchemaVersion = 1

// wantContract reports whether the process was asked to print its contract
// instead of serving.
//
// Hand-rolled rather than the flag package on purpose. Config here is
// environment-only -- see the "no flags for anything a chart sets" rule in
// docs/conventions.md -- and installing a FlagSet would put a flag surface on a
// binary that deliberately has none. A contract dump is not configuration: it
// is a second thing this binary can do, and it should read as one.
func wantContract(args []string) bool {
	for _, a := range args {
		if a == "-contract" || a == "--contract" {
			return true
		}
	}
	return false
}

// contract prints this server's published interface as JSON on stdout.
//
// "Run the app" rather than parse it: the tool schemas below come from a real
// tools/list round trip against a real server over an in-memory transport, so
// they are whatever the SDK actually infers from the handler signatures. A
// hand-maintained copy of those schemas would be a second source of truth, and
// the whole point of a generated contract is that there is only one.
func contract(ctx context.Context, w io.Writer) error {
	// A dump must not depend on the environment of whoever ran it, or two
	// machines produce two different contracts for one commit. Clearing the
	// prefix and then calling the real loader gives the shipped defaults
	// without duplicating them here.
	restore := clearEnvPrefix("FATHOM_MCP_")
	defer restore()

	base, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading default config: %w", err)
	}

	always, err := listTools(ctx, base)
	if err != nil {
		return err
	}

	// The second pass is not optional. The metrics tools are gated on
	// prometheus.url, so a contract taken from one deployment describes the
	// other one wrongly -- and "these three tools exist but only when
	// configured" is exactly the fact a consumer needs. Any URL will do; no
	// Prometheus client is built on this path.
	os.Setenv("FATHOM_MCP_PROMETHEUS_URL", "http://prometheus.invalid")
	gatedCfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading metrics-enabled config: %w", err)
	}
	withMetrics, err := listTools(ctx, gatedCfg)
	if err != nil {
		return err
	}

	doc := map[string]any{
		"schema_version": contractSchemaVersion,
		"component":      "mcp",
		"service":        "fathom-mcp",
		"protocol": map[string]any{
			"kind":      "mcp",
			"transport": "streamable-http",
			"path":      base.MCPPath,
			// Registered twice so /mcp/sub paths match; both are the same
			// handler, and a consumer needs to know the subtree form works.
			"paths": []string{base.MCPPath, base.MCPPath + "/"},
			"instructions": map[string]string{
				"without_prometheus": instructions(false),
				"with_prometheus":    instructions(true),
			},
		},
		"probes": map[string]string{
			"liveness":  "/healthz",
			"readiness": "/readyz",
		},
		"tools": map[string]any{
			"always": always,
			"gated": map[string]any{
				"on":       "FATHOM_MCP_PROMETHEUS_URL",
				"helm_key": "prometheus.url",
				"reason": "A tool that fails on every call is worse than an absent one: " +
					"the model spends turns discovering it and reads the failure as an " +
					"outage rather than a deployment choice.",
				"tools": onlyIn(withMetrics, always),
			},
		},
		"defaults": map[string]any{
			"addr":               base.Addr,
			"allowed_resources":  base.AllowedResources,
			"allowed_namespaces": base.AllowedNamespaces,
			"request_timeout":    base.RequestTimeout.String(),
		},
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// listTools builds a server exactly as run() does, connects an in-memory
// client to it and asks it what tools it has.
//
// Two deliberate differences from the serving path, both because this is not
// serving. The Kubernetes and Prometheus clients are nil: Register only
// declares tools and never dials either, so a dump needs no cluster. And the
// receiving middleware is not installed -- it drops requests that carry no
// trace context, which is precisely what this tools/list is.
func listTools(ctx context.Context, cfg *config.Config) ([]*mcp.Tool, error) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))

	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "fathom-mcp",
		Version: version,
	}, &mcp.ServerOptions{
		Instructions: instructions(cfg.MetricsEnabled()),
	})
	tools.New(cfg, nil, nil, log).Register(srv)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		return nil, fmt.Errorf("connecting contract server: %w", err)
	}
	defer serverSession.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "fathom-contract", Version: version}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		return nil, fmt.Errorf("connecting contract client: %w", err)
	}
	defer clientSession.Close()

	res, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	return res.Tools, nil
}

// onlyIn returns the tools present in got but not in base, which is how the
// gated group is identified: it is the difference the Prometheus URL makes.
func onlyIn(got, base []*mcp.Tool) []*mcp.Tool {
	seen := make(map[string]bool, len(base))
	for _, t := range base {
		seen[t.Name] = true
	}
	out := make([]*mcp.Tool, 0, len(got)-len(base))
	for _, t := range got {
		if !seen[t.Name] {
			out = append(out, t)
		}
	}
	return out
}

// clearEnvPrefix unsets every variable with the given prefix and returns a
// function that puts them back, so a dump reports the shipped defaults rather
// than the operator's shell.
func clearEnvPrefix(prefix string) func() {
	saved := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if ok && strings.HasPrefix(k, prefix) {
			saved[k] = v
			os.Unsetenv(k)
		}
	}
	return func() {
		for k, v := range saved {
			os.Setenv(k, v)
		}
	}
}
