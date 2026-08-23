package config

import (
	"fmt"
	"log/slog"
	"net/url"
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

	// PrometheusURL is the root of the Prometheus HTTP API, e.g.
	// "http://prometheus.observability.svc.cluster.local:9090".
	//
	// Empty is the meaningful default: with no URL there is no Prometheus
	// client and the three metrics tools are never registered, so a
	// deployment without a metrics backend advertises only the Kubernetes
	// tools rather than advertising tools that would always fail.
	PrometheusURL string

	// PrometheusTimeout bounds a single call into Prometheus. Separate from
	// RequestTimeout because it is a different backend with a different
	// failure profile -- a PromQL evaluation can be slow for reasons a
	// Kubernetes read never is.
	PrometheusTimeout time.Duration

	// Metrics tool bounds.
	//
	// MetricsListLimit caps the metric names list_metrics returns.
	// MetricsSeriesLimit caps the series describe_metric inspects, and doubles
	// as the point past which cardinality is reported as ">=" rather than
	// exactly. MetricsLabelValues caps the distinct values shown per label.
	MetricsListLimit   int
	MetricsSeriesLimit int
	MetricsLabelValues int

	// PromQL bounds. MaxSeries caps the elements rendered from a query result;
	// MaxQueryLength refuses an oversized expression before it is sent.
	PromQLMaxSeries      int
	PromQLMaxQueryLength int

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
		PrometheusURL:     strings.TrimSpace(env("PROMETHEUS_URL", "")),
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
	if c.PrometheusTimeout, err = envDuration("PROMETHEUS_TIMEOUT", 15*time.Second); err != nil {
		return nil, err
	}
	if c.MetricsListLimit, err = envInt("METRICS_LIST_LIMIT", 500); err != nil {
		return nil, err
	}
	if c.MetricsSeriesLimit, err = envInt("METRICS_SERIES_LIMIT", 500); err != nil {
		return nil, err
	}
	if c.MetricsLabelValues, err = envInt("METRICS_LABEL_VALUES", 50); err != nil {
		return nil, err
	}
	if c.PromQLMaxSeries, err = envInt("PROMQL_MAX_SERIES", 100); err != nil {
		return nil, err
	}
	if c.PromQLMaxQueryLength, err = envInt("PROMQL_MAX_QUERY_LENGTH", 2048); err != nil {
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
	// An unset URL is valid and means "metrics tools disabled". A set but
	// unusable one is a deploy mistake, and failing fast here is what turns it
	// into a named error in the first line of the pod's logs rather than a
	// tool that errors on every call.
	if c.PrometheusURL != "" {
		u, err := url.Parse(c.PrometheusURL)
		switch {
		case err != nil:
			return fmt.Errorf("%sPROMETHEUS_URL: %w", envPrefix, err)
		case u.Scheme != "http" && u.Scheme != "https":
			return fmt.Errorf("%sPROMETHEUS_URL: want an http:// or https:// URL, got %q",
				envPrefix, c.PrometheusURL)
		case u.Host == "":
			return fmt.Errorf("%sPROMETHEUS_URL: %q has no host", envPrefix, c.PrometheusURL)
		}
	}
	if c.PrometheusTimeout <= 0 {
		return fmt.Errorf("%sPROMETHEUS_TIMEOUT must be > 0, got %s", envPrefix, c.PrometheusTimeout)
	}
	for _, b := range []struct {
		name  string
		value int
	}{
		{"METRICS_LIST_LIMIT", c.MetricsListLimit},
		{"METRICS_SERIES_LIMIT", c.MetricsSeriesLimit},
		{"METRICS_LABEL_VALUES", c.MetricsLabelValues},
		{"PROMQL_MAX_SERIES", c.PromQLMaxSeries},
		{"PROMQL_MAX_QUERY_LENGTH", c.PromQLMaxQueryLength},
	} {
		if b.value <= 0 {
			return fmt.Errorf("%s%s must be > 0, got %d", envPrefix, b.name, b.value)
		}
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

// MetricsEnabled reports whether a Prometheus backend is configured, and so
// whether list_metrics, describe_metric and run_promql are registered at all.
func (c *Config) MetricsEnabled() bool { return c.PrometheusURL != "" }

// VarDoc is one environment variable this component reads.
type VarDoc struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Default string `json:"default"`
}

// declared accumulates a VarDoc per lookup, appended by the helpers below.
//
// Recording here rather than in a table beside Load() is the whole point: every
// variable this component reads goes through one of these functions, so what is
// recorded is what is read. A separate table would be a second source of truth
// and would drift the first time someone added a variable without updating it.
var declared []VarDoc

// Describe returns the variables Load reads, in declaration order and without
// duplicates. Meaningful only after Load has run at least once.
func Describe() []VarDoc {
	seen := map[string]bool{}
	out := make([]VarDoc, 0, len(declared))
	for _, d := range declared {
		if !seen[d.Name] {
			seen[d.Name] = true
			out = append(out, d)
		}
	}
	return out
}

func record(name, typ, def string) {
	declared = append(declared, VarDoc{Name: envPrefix + name, Type: typ, Default: def})
}

func lookup(name string) (string, bool) {
	return os.LookupEnv(envPrefix + name)
}

func env(name, def string) string {
	record(name, "string", def)
	if v, ok := lookup(name); ok {
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

// envInt delegates, so the recording happens once, in envInt64.
func envInt(name string, def int) (int, error) {
	v, err := envInt64(name, int64(def))
	return int(v), err
}

func envInt64(name string, def int64) (int64, error) {
	record(name, "int", strconv.FormatInt(def, 10))
	raw, ok := lookup(name)
	if !ok || raw == "" {
		return def, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s%s: %w", envPrefix, name, err)
	}
	return v, nil
}

func envDuration(name string, def time.Duration) (time.Duration, error) {
	record(name, "duration", def.String())
	raw, ok := lookup(name)
	if !ok || raw == "" {
		return def, nil
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s%s: %w", envPrefix, name, err)
	}
	return v, nil
}

func envLevel(name string, def slog.Level) (slog.Level, error) {
	record(name, "log level", def.String())
	raw, ok := lookup(name)
	if !ok || raw == "" {
		return def, nil
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(raw)); err != nil {
		return 0, fmt.Errorf("%s%s: %w", envPrefix, name, err)
	}
	return l, nil
}
