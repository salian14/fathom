package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

const envPrefix = "FATHOM_MCP_"

type Config struct {
	// Transport is "http" (streamable HTTP, how the agent pod connects) or
	// "stdio" (a local process on a developer's machine).
	Transport string

	// Addr is the listen address for the HTTP transport.
	Addr string

	// MCPPath is the HTTP path the MCP endpoint is served on. Health probes are
	// served alongside it on /healthz and /readyz.
	MCPPath string

	// KubeconfigPath is a *fallback* only, for running outside the cluster.
	// In-cluster credentials (projected token + CA) are always tried first.
	KubeconfigPath string

	// AllowedResources is the allowlist of resource types the tools will touch,
	// as canonical "resource" or "resource.group" strings (e.g. "pods",
	// "deployments.apps"). Anything not listed is refused before a request
	// reaches the API server.
	//
	// This list is generated from the same Helm value that generates the
	// ClusterRole, which is what keeps the tool surface and the RBAC in
	// lockstep. Adding a kind in one place without the other is a deploy-time
	// change, not a code change.
	AllowedResources []string

	// AllowedNamespaces optionally restricts every namespaced read to these
	// namespaces. Empty means "any namespace the RBAC permits".
	AllowedNamespaces []string

	// DefaultNamespace is used when the model omits a namespace on a namespaced
	// resource.
	DefaultNamespace string

	// ListLimit caps the number of items returned by an unnamed k8s_get.
	ListLimit int64

	// MaxBytes caps the size of any single tool result. The model's context is
	// the scarce resource here (see ollama/config.env: FATHOM_NUM_CTX).
	MaxBytes int

	// Log tool bounds. TailLines is the default; MaxTailLines is the ceiling the
	// model may request.
	LogTailLines    int64
	LogMaxTailLines int64
	LogMaxBytes     int

	// RequestTimeout bounds a single call into the Kubernetes API.
	RequestTimeout time.Duration

	// LogLevel for the server's own structured logs.
	LogLevel slog.Level

	// Telemetry selects where traces and metrics go: "otlp" to the collector
	// named by OTEL_EXPORTER_OTLP_ENDPOINT, "console" to stderr, or "off".
	//
	// This is the only telemetry setting with a FATHOM_MCP_ prefix. Everything
	// else -- endpoint, sampler, resource attributes, export interval -- uses
	// the standard OTEL_* variables, which the SDK reads directly. Mirroring
	// those here would duplicate the SDK's own validation and give every knob
	// two spellings; this one earns its place because it decides which
	// exporter gets built, and because "console" is what makes running the
	// server locally pleasant.
	Telemetry string
}

var defaultAllowedResources = []string{
	"pods",
	"services",
	"nodes",
	"namespaces",
	"events",
	"configmaps",
	"persistentvolumeclaims",
	"serviceaccounts",
	"endpoints",
	"replicationcontrollers",
	"deployments.apps",
	"replicasets.apps",
	"statefulsets.apps",
	"daemonsets.apps",
	"jobs.batch",
	"cronjobs.batch",
	"ingresses.networking.k8s.io",
	"networkpolicies.networking.k8s.io",
}

func Load() (*Config, error) {
	c := &Config{
		Transport:         env("TRANSPORT", "http"),
		Addr:              env("ADDR", ":8080"),
		MCPPath:           env("MCP_PATH", "/mcp"),
		KubeconfigPath:    env("KUBECONFIG_PATH", ""),
		AllowedResources:  csv(env("ALLOWED_RESOURCES", strings.Join(defaultAllowedResources, ","))),
		AllowedNamespaces: csv(env("ALLOWED_NAMESPACES", "")),
		DefaultNamespace:  env("DEFAULT_NAMESPACE", "default"),
		Telemetry:         env("TELEMETRY", "otlp"),
	}

	var err error
	if c.ListLimit, err = envInt64("LIST_LIMIT", 200); err != nil {
		return nil, err
	}
	if c.MaxBytes, err = envInt("MAX_BYTES", 64*1024); err != nil {
		return nil, err
	}
	if c.LogTailLines, err = envInt64("LOG_TAIL_LINES", 200); err != nil {
		return nil, err
	}
	if c.LogMaxTailLines, err = envInt64("LOG_MAX_TAIL_LINES", 2000); err != nil {
		return nil, err
	}
	if c.LogMaxBytes, err = envInt("LOG_MAX_BYTES", 32*1024); err != nil {
		return nil, err
	}
	if c.RequestTimeout, err = envDuration("REQUEST_TIMEOUT", 30*time.Second); err != nil {
		return nil, err
	}
	if c.LogLevel, err = envLevel("LOG_LEVEL", slog.LevelInfo); err != nil {
		return nil, err
	}

	return c, c.validate()
}

func (c *Config) validate() error {
	switch c.Transport {
	case "http", "stdio":
	default:
		return fmt.Errorf("%sTRANSPORT: want \"http\" or \"stdio\", got %q", envPrefix, c.Transport)
	}
	if c.Transport == "http" && c.Addr == "" {
		return fmt.Errorf("%sADDR must be set when TRANSPORT=http", envPrefix)
	}
	if !strings.HasPrefix(c.MCPPath, "/") {
		return fmt.Errorf("%sMCP_PATH must start with \"/\", got %q", envPrefix, c.MCPPath)
	}
	if len(c.AllowedResources) == 0 {
		return fmt.Errorf("%sALLOWED_RESOURCES is empty: the server would refuse every request", envPrefix)
	}
	for _, r := range c.AllowedResources {
		if strings.ContainsAny(r, "/ ") {
			return fmt.Errorf("%sALLOWED_RESOURCES: %q is not a bare resource or resource.group "+
				"(subresources are granted by the chart, not listed here)", envPrefix, r)
		}
	}
	if c.ListLimit <= 0 {
		return fmt.Errorf("%sLIST_LIMIT must be > 0, got %d", envPrefix, c.ListLimit)
	}
	if c.MaxBytes <= 0 {
		return fmt.Errorf("%sMAX_BYTES must be > 0, got %d", envPrefix, c.MaxBytes)
	}
	if c.LogTailLines <= 0 || c.LogMaxTailLines <= 0 {
		return fmt.Errorf("%sLOG_TAIL_LINES and %sLOG_MAX_TAIL_LINES must be > 0", envPrefix, envPrefix)
	}
	if c.LogTailLines > c.LogMaxTailLines {
		return fmt.Errorf("%sLOG_TAIL_LINES (%d) exceeds %sLOG_MAX_TAIL_LINES (%d)",
			envPrefix, c.LogTailLines, envPrefix, c.LogMaxTailLines)
	}
	if c.LogMaxBytes <= 0 {
		return fmt.Errorf("%sLOG_MAX_BYTES must be > 0, got %d", envPrefix, c.LogMaxBytes)
	}
	if c.RequestTimeout <= 0 {
		return fmt.Errorf("%sREQUEST_TIMEOUT must be > 0, got %s", envPrefix, c.RequestTimeout)
	}
	switch c.Telemetry {
	case "otlp", "console", "off":
	default:
		return fmt.Errorf("%sTELEMETRY: want \"otlp\", \"console\" or \"off\", got %q",
			envPrefix, c.Telemetry)
	}
	return nil
}

func (c *Config) AllowsNamespace(ns string) bool {
	if len(c.AllowedNamespaces) == 0 {
		return true
	}
	for _, a := range c.AllowedNamespaces {
		if a == ns {
			return true
		}
	}
	return false
}

func (c *Config) AllowsResource(key string) bool {
	for _, a := range c.AllowedResources {
		if a == key {
			return true
		}
	}
	return false
}

func env(name, def string) string {
	if v, ok := os.LookupEnv(envPrefix + name); ok {
		return v
	}
	return def
}

func csv(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envInt(name string, def int) (int, error) {
	v, err := envInt64(name, int64(def))
	return int(v), err
}

func envInt64(name string, def int64) (int64, error) {
	raw := env(name, "")
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s%s: %w", envPrefix, name, err)
	}
	return v, nil
}

func envDuration(name string, def time.Duration) (time.Duration, error) {
	raw := env(name, "")
	if raw == "" {
		return def, nil
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s%s: %w", envPrefix, name, err)
	}
	return v, nil
}

func envLevel(name string, def slog.Level) (slog.Level, error) {
	raw := env(name, "")
	if raw == "" {
		return def, nil
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(raw)); err != nil {
		return 0, fmt.Errorf("%s%s: %w", envPrefix, name, err)
	}
	return l, nil
}
