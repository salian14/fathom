package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatalf("Load() with no environment: %v", err)
	}

	if c.Addr != ":8080" {
		t.Errorf("Addr = %q, want \":8080\"", c.Addr)
	}
	if c.AgentURL != "http://fathom-agent.fathom.svc.cluster.local:8080" {
		t.Errorf("AgentURL = %q", c.AgentURL)
	}
	if c.ChatURL() != "http://fathom-agent.fathom.svc.cluster.local:8080/chat" {
		t.Errorf("ChatURL() = %q", c.ChatURL())
	}
	if c.ReadyURL() != "http://fathom-agent.fathom.svc.cluster.local:8080/readyz" {
		t.Errorf("ReadyURL() = %q", c.ReadyURL())
	}
	// The whole point of this default: it has to outlast the agent's own 180s
	// budget so the agent's 504 is what reaches the browser.
	if c.AgentTimeout <= 180*time.Second {
		t.Errorf("AgentTimeout = %s, want > 180s so the agent times out first", c.AgentTimeout)
	}
	if c.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", c.LogLevel)
	}
	if c.Telemetry != "otlp" {
		t.Errorf("Telemetry = %q, want \"otlp\"", c.Telemetry)
	}
}

func TestAgentURLTrailingSlash(t *testing.T) {
	t.Setenv("FATHOM_FRONT_AGENT_URL", "http://localhost:8081/")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if c.ChatURL() != "http://localhost:8081/chat" {
		t.Errorf("ChatURL() = %q, want no doubled slash", c.ChatURL())
	}
}

func TestLoadRejects(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string // substring the error must name
	}{
		{"addr without port", map[string]string{"FATHOM_FRONT_ADDR": "8080"}, "ADDR"},
		{"addr port out of range", map[string]string{"FATHOM_FRONT_ADDR": ":99999"}, "ADDR"},
		{"agent url without scheme", map[string]string{"FATHOM_FRONT_AGENT_URL": "fathom-agent:8080"}, "AGENT_URL"},
		{"agent url without host", map[string]string{"FATHOM_FRONT_AGENT_URL": "http://"}, "AGENT_URL"},
		{"unparseable timeout", map[string]string{"FATHOM_FRONT_AGENT_TIMEOUT": "soon"}, "AGENT_TIMEOUT"},
		{"zero timeout", map[string]string{"FATHOM_FRONT_AGENT_TIMEOUT": "0s"}, "AGENT_TIMEOUT"},
		{"ready timeout past agent timeout", map[string]string{
			"FATHOM_FRONT_AGENT_TIMEOUT": "5s",
			"FATHOM_FRONT_READY_TIMEOUT": "30s",
		}, "READY_TIMEOUT"},
		{"negative body cap", map[string]string{"FATHOM_FRONT_MAX_BODY_BYTES": "-1"}, "MAX_BODY_BYTES"},
		{"unknown telemetry mode", map[string]string{"FATHOM_FRONT_TELEMETRY": "jaeger"}, "TELEMETRY"},
		{"unknown log level", map[string]string{"FATHOM_FRONT_LOG_LEVEL": "chatty"}, "LOG_LEVEL"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			_, err := Load()
			if err == nil {
				t.Fatal("Load() succeeded, want an error")
			}
			// Every message names the variable at fault: the first line of a
			// crashed pod's logs should say what to fix, not that something is
			// wrong.
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not name %s", err, tt.want)
			}
		})
	}
}

func TestLoadAccepts(t *testing.T) {
	t.Setenv("FATHOM_FRONT_ADDR", "127.0.0.1:9000")
	t.Setenv("FATHOM_FRONT_AGENT_URL", "https://agent.example:8443")
	t.Setenv("FATHOM_FRONT_AGENT_TIMEOUT", "90s")
	t.Setenv("FATHOM_FRONT_READY_TIMEOUT", "2s")
	t.Setenv("FATHOM_FRONT_MAX_BODY_BYTES", "1024")
	t.Setenv("FATHOM_FRONT_LOG_LEVEL", "debug")
	t.Setenv("FATHOM_FRONT_TELEMETRY", "off")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if c.Addr != "127.0.0.1:9000" || c.AgentTimeout != 90*time.Second ||
		c.ReadyTimeout != 2*time.Second || c.MaxBodyBytes != 1024 ||
		c.LogLevel != slog.LevelDebug || c.Telemetry != "off" {
		t.Errorf("Load() = %+v, does not reflect the environment", c)
	}
}
