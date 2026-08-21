package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spellbound/fathom/fathom-front/internal/config"
)

func testConfig(agentURL string) *config.Config {
	return &config.Config{
		AgentURL:     agentURL,
		AgentTimeout: 5 * time.Second,
		ReadyTimeout: 2 * time.Second,
	}
}

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestReadyWhenAgentIsReady(t *testing.T) {
	var gotPath string
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, "ok\n")
	}))
	defer agent.Close()

	rec := httptest.NewRecorder()
	readyHandler(testConfig(agent.URL), quietLog())(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if gotPath != "/readyz" {
		t.Errorf("agent saw %q, want \"/readyz\"", gotPath)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// The agent's 503 body names which dependency failed -- "ollama unreachable at
// ..." or "mcp server unreachable". Forwarding it verbatim is what keeps
// `kubectl describe pod` on this service useful instead of merely saying no.
func TestNotReadyForwardsTheAgentsReason(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "ollama unreachable at http://192.168.56.1:11434: timed out\n")
	}))
	defer agent.Close()

	rec := httptest.NewRecorder()
	readyHandler(testConfig(agent.URL), quietLog())(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ollama unreachable") {
		t.Errorf("body = %q, want the agent's own reason", rec.Body.String())
	}
}

func TestNotReadyWhenAgentIsAbsent(t *testing.T) {
	rec := httptest.NewRecorder()
	readyHandler(testConfig("http://127.0.0.1:1"), quietLog())(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "agent unreachable") {
		t.Errorf("body = %q, want it to name the agent", rec.Body.String())
	}
}
