// Package config loads fathom-front's settings from the environment and
// refuses to start on a bad one.
//
// Same shape as mcp/internal/config: FATHOM_FRONT_-prefixed variables, defaults
// that work in-cluster, and validation that names the offending variable. A
// misconfigured front end is a first-line-of-the-logs problem, not a
// discovered-at-request-time one.
package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const envPrefix = "FATHOM_FRONT_"

// addrPattern matches the agent's own FATHOM_AGENT_ADDR rule, so ":8080" and
// "127.0.0.1:8080" are spelled identically across the two services.
var addrPattern = regexp.MustCompile(`^[^:]*:\d+$`)

type Config struct {
	// Addr is the listen address for the UI and the proxy alike. They share a
	// port on purpose: same-origin is what lets the browser call /api/chat
	// without the agent growing CORS middleware.
	Addr string

	// AgentURL is the base URL of fathom-agent. The proxy appends /chat and
	// the readiness handler appends /readyz; neither path is configurable,
	// because they are the agent's API rather than a deployment detail.
	AgentURL string

	// AgentTimeout bounds one proxied turn. It must exceed the agent's own
	// FATHOM_AGENT_REQUEST_TIMEOUT (180s by default) so the agent's 504 --
	// which says how long it waited -- reaches the browser, instead of this
	// service timing out first and reporting a less useful failure.
	AgentTimeout time.Duration

	// ReadyTimeout bounds the /readyz call into the agent. Short: the agent
	// caches its own readiness for 30s, so this never waits on a real probe.
	ReadyTimeout time.Duration

	// MaxBodyBytes caps an inbound /api/chat request. A question plus a capped
	// transcript is kilobytes; anything far past that is a mistake or an abuse.
	MaxBodyBytes int64

	// EvalURL is the base URL of fathom-eval's read service, e.g.
	// "http://fathom-eval-serve.fathom-eval.svc.cluster.local:8080".
	//
	// Empty DISABLES the eval view entirely: no /api/eval route is registered
	// and the page shows no switcher. Same gate shape as fathom-mcp's
	// prometheus.url and for the same reason -- a feature that fails on every
	// use is worse than one that is not there, because the reader spends time
	// discovering it is broken rather than knowing it is absent. Empty is the
	// shipped default: eval is optional, and most installs will not have it.
	EvalURL string

	// EvalTimeout bounds one proxied read. Deliberately not AgentTimeout: that
	// is 200s because a model turn takes minutes, while this is a SELECT
	// against a local SQLite file and answers in milliseconds. Sharing the knob
	// would mean a wedged reader holding a browser connection for three
	// minutes.
	EvalTimeout time.Duration

	// LogLevel for this service's own structured logs.
	LogLevel slog.Level

	// Telemetry selects where traces and metrics go: "otlp", "console" or
	// "off". Same vocabulary as fathom-agent and fathom-mcp -- one word to
	// learn, not three.
	Telemetry string
}

func Load() (*Config, error) {
	c := &Config{
		Addr:      env("ADDR", ":8080"),
		AgentURL:  strings.TrimRight(env("AGENT_URL", "http://fathom-agent.fathom.svc.cluster.local:8080"), "/"),
		EvalURL:   strings.TrimRight(env("EVAL_URL", ""), "/"),
		Telemetry: env("TELEMETRY", "otlp"),
	}

	var err error
	if c.AgentTimeout, err = envDuration("AGENT_TIMEOUT", 200*time.Second); err != nil {
		return nil, err
	}
	if c.ReadyTimeout, err = envDuration("READY_TIMEOUT", 5*time.Second); err != nil {
		return nil, err
	}
	if c.EvalTimeout, err = envDuration("EVAL_TIMEOUT", 10*time.Second); err != nil {
		return nil, err
	}
	if c.MaxBodyBytes, err = envInt64("MAX_BODY_BYTES", 256*1024); err != nil {
		return nil, err
	}
	if c.LogLevel, err = envLevel("LOG_LEVEL", slog.LevelInfo); err != nil {
		return nil, err
	}

	return c, c.validate()
}

func (c *Config) validate() error {
	if !addrPattern.MatchString(c.Addr) {
		return fmt.Errorf("%sADDR: want \"host:port\" or \":port\", got %q", envPrefix, c.Addr)
	}
	if port, err := strconv.Atoi(c.Addr[strings.LastIndex(c.Addr, ":")+1:]); err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("%sADDR: port must be 1-65535, got %q", envPrefix, c.Addr)
	}

	// Fail here rather than on the first request. An unreachable agent is an
	// operational problem the readiness probe reports; an unparseable URL is a
	// deploy mistake, and it should keep the pod from ever going ready.
	u, err := url.Parse(c.AgentURL)
	switch {
	case err != nil:
		return fmt.Errorf("%sAGENT_URL: %w", envPrefix, err)
	case u.Scheme != "http" && u.Scheme != "https":
		return fmt.Errorf("%sAGENT_URL: want an http:// or https:// URL, got %q", envPrefix, c.AgentURL)
	case u.Host == "":
		return fmt.Errorf("%sAGENT_URL: %q has no host", envPrefix, c.AgentURL)
	}

	if c.AgentTimeout <= 0 {
		return fmt.Errorf("%sAGENT_TIMEOUT must be > 0, got %s", envPrefix, c.AgentTimeout)
	}
	if c.ReadyTimeout <= 0 {
		return fmt.Errorf("%sREADY_TIMEOUT must be > 0, got %s", envPrefix, c.ReadyTimeout)
	}
	if c.ReadyTimeout > c.AgentTimeout {
		return fmt.Errorf("%sREADY_TIMEOUT (%s) exceeds %sAGENT_TIMEOUT (%s)",
			envPrefix, c.ReadyTimeout, envPrefix, c.AgentTimeout)
	}
	// Only when set. Empty is the disabled state, not a misconfiguration.
	if c.EvalURL != "" {
		u, err := url.Parse(c.EvalURL)
		switch {
		case err != nil:
			return fmt.Errorf("%sEVAL_URL: %w", envPrefix, err)
		case u.Scheme != "http" && u.Scheme != "https":
			return fmt.Errorf("%sEVAL_URL: want an http:// or https:// URL, got %q", envPrefix, c.EvalURL)
		case u.Host == "":
			return fmt.Errorf("%sEVAL_URL: %q has no host", envPrefix, c.EvalURL)
		}
	}
	if c.EvalTimeout <= 0 {
		return fmt.Errorf("%sEVAL_TIMEOUT must be > 0, got %s", envPrefix, c.EvalTimeout)
	}
	if c.MaxBodyBytes <= 0 {
		return fmt.Errorf("%sMAX_BODY_BYTES must be > 0, got %d", envPrefix, c.MaxBodyBytes)
	}
	switch c.Telemetry {
	case "otlp", "console", "off":
	default:
		return fmt.Errorf("%sTELEMETRY: want \"otlp\", \"console\" or \"off\", got %q",
			envPrefix, c.Telemetry)
	}
	return nil
}

// ChatURL and ReadyURL are the two agent endpoints this service talks to.
func (c *Config) ChatURL() string  { return c.AgentURL + "/chat" }
func (c *Config) ReadyURL() string { return c.AgentURL + "/readyz" }

// EvalEnabled reports whether the eval view is configured. There is no
// EvalAPIURL() to go with it: the base URL *is* the proxy target, and mapping
// /api/eval/x onto /x is the proxy's StripPrefix, not a string built here.
func (c *Config) EvalEnabled() bool { return c.EvalURL != "" }

// VarDoc is one environment variable this component reads.
type VarDoc struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Default string `json:"default"`
}

// declared accumulates a VarDoc per lookup, appended by the helpers below.
//
// Recording here rather than in a table beside Load() is the whole point: every
// variable this component reads goes through one of these four functions, so what
// is recorded is what is read. A separate table would be a second source of truth
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
