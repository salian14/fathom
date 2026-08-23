package main

import (
	"encoding/json"
	"fmt"
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

// --- the mux, and the eval gate ---------------------------------------------

func stubHandler(hit *int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*hit++
		w.WriteHeader(http.StatusOK)
	})
}

func muxFor(t *testing.T, evalURL string, eval http.Handler) http.Handler {
	t.Helper()
	cfg := testConfig("http://127.0.0.1:1")
	cfg.EvalURL = evalURL
	return newMux(cfg, handlers{
		chat: stubHandler(new(int)),
		eval: eval,
		ui:   http.NotFoundHandler(),
	}, quietLog())
}

// TestReadyzDoesNotDependOnEval encodes an operational decision, not a detail.
//
// /readyz gates this pod's membership of its Service. If the eval read view
// being down took fathom-front out of the Service, an unavailable *inspection*
// page would take the *chat* offline with it. The eval view reports its own
// failures, in its own pane.
func TestReadyzDoesNotDependOnEval(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	}))
	defer agent.Close()

	cfg := testConfig(agent.URL)
	cfg.EvalURL = "http://127.0.0.1:1" // nothing is listening there
	var hit int
	mux := newMux(cfg, handlers{chat: stubHandler(&hit), eval: stubHandler(&hit), ui: http.NotFoundHandler()}, quietLog())

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("/readyz = %d with eval unreachable, want 200: %s", rec.Code, rec.Body)
	}
}

func TestEvalRouteAbsentWhenDisabled(t *testing.T) {
	mux := muxFor(t, "", nil)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/eval/runs", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("/api/eval/runs = %d with eval disabled, want 404", rec.Code)
	}
	// It must really be absent, not a proxy failing: a registered route that
	// errors would return the {"error": ...} envelope instead.
	if strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("body = %q, want the file server's 404 rather than a proxy error", rec.Body)
	}
}

func TestEvalRouteProxiesWhenEnabled(t *testing.T) {
	var hit int
	mux := muxFor(t, "http://eval.invalid:8080", stubHandler(&hit))

	for _, path := range []string{"/api/eval/runs", "/api/eval/runs/12", "/api/eval/results/941"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s = %d, want 200", path, rec.Code)
		}
	}
	if hit != 3 {
		t.Errorf("eval handler hit %d times, want 3", hit)
	}
}

func TestFeaturesReportsTheGate(t *testing.T) {
	for _, tt := range []struct {
		name    string
		evalURL string
		want    bool
	}{
		{"disabled", "", false},
		{"enabled", "http://eval.invalid:8080", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var hit int
			var eval http.Handler
			if tt.want {
				eval = stubHandler(&hit)
			}
			mux := muxFor(t, tt.evalURL, eval)

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/features", nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("/api/features = %d, want 200", rec.Code)
			}
			var got map[string]bool
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("unmarshal %q: %v", rec.Body, err)
			}
			if got["eval"] != tt.want {
				t.Errorf("features.eval = %v, want %v", got["eval"], tt.want)
			}
		})
	}
}

func TestSpanPathCollapsesIds(t *testing.T) {
	// Without this, one span name per result ever viewed.
	for _, tt := range []struct{ in, want string }{
		{"/api/eval/results/941", "/api/eval/results/{id}"},
		{"/api/eval/runs/12", "/api/eval/runs/{id}"},
		{"/api/eval/runs", "/api/eval/runs"},
		{"/api/chat", "/api/chat"},
		{"/", "/"},
	} {
		if got := spanPath(tt.in); got != tt.want {
			t.Errorf("spanPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestEveryRouteIsRegistered is the guard that makes routes() safe to publish.
//
// routes() is both the routing table and the source of the generated contract,
// so the failure it has to rule out is an entry that describes a route nobody
// serves -- a contract that lies is worse than no contract, because it is
// believed. Every entry is dialled through the real mux with the eval gate
// open, and anything that does not reach its handler is a lie in the spec.
func TestEveryRouteIsRegistered(t *testing.T) {
	var chat, eval int
	cfg := testConfig("http://127.0.0.1:1")
	cfg.EvalURL = "http://eval.invalid:8080"
	mux := newMux(cfg, handlers{
		chat: stubHandler(&chat),
		eval: stubHandler(&eval),
		ui:   stubHandler(new(int)),
	}, quietLog())

	for _, r := range routes() {
		// A subtree pattern is not itself a request path; dial something under
		// it, which is the form a browser actually sends.
		path := r.Pattern
		if strings.HasSuffix(path, "/") && path != "/" {
			path += "probe"
		}
		for _, method := range r.Methods {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
			if rec.Code == http.StatusNotFound {
				t.Errorf("%s %s: 404, so routes() describes a route the mux does not serve", method, path)
			}
		}
		if len(r.Methods) == 0 {
			t.Errorf("routes() entry %q declares no methods, so the contract cannot describe it", r.Pattern)
		}
	}
}

// TestGatedRouteIsMarkedInTheTable keeps the contract's gate honest: the entry
// that disappears without a URL is the one that must carry GatedOn, or the
// generated spec advertises a route that is absent on most installs.
func TestGatedRouteIsMarkedInTheTable(t *testing.T) {
	for _, r := range routes() {
		gatedInTable := r.GatedOn != ""
		conditional := r.enabled != nil
		if gatedInTable != conditional {
			t.Errorf("route %q: GatedOn=%q but enabled==nil is %v -- the table and the gate disagree",
				r.Pattern, r.GatedOn, r.enabled == nil)
		}
	}
}
