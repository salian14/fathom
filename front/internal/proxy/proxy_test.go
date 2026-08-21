package proxy

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestProxy(t *testing.T, target string, opts ...func(*Options)) http.Handler {
	t.Helper()
	o := Options{
		Target:       target,
		Timeout:      5 * time.Second,
		MaxBodyBytes: 1024,
		Log:          quietLog(),
	}
	for _, f := range opts {
		f(&o)
	}
	h, err := New(o)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	return h
}

// The proxy must land on the agent's /chat exactly, not on /api/chat and not
// on /chat/api/chat -- ReverseProxy.SetURL joins paths by default, which is
// the mistake this guards.
func TestForwardsToChatPath(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answer":"hello","steps":[],"iterations":0}`))
	}))
	defer agent.Close()

	h := newTestProxy(t, agent.URL+"/chat")

	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"question":"hi"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if gotPath != "/chat" {
		t.Errorf("agent saw path %q, want \"/chat\"", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("agent saw method %q, want POST", gotMethod)
	}
	if gotBody != `{"question":"hi"}` {
		t.Errorf("agent saw body %q, want it forwarded unchanged", gotBody)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"answer":"hello"`) {
		t.Errorf("body = %q, want the agent's response verbatim", rec.Body.String())
	}
}

// The agent's error vocabulary is the UI's error vocabulary. Nothing here
// rewrites a status, because a 504 ("the turn ran long") and a 502 ("Ollama is
// down") lead to different fixes and collapsing them loses that.
func TestPassesAgentStatusThrough(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity,
		http.StatusBadGateway, http.StatusGatewayTimeout} {
		agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"turn exceeded 180s"}`))
		}))

		h := newTestProxy(t, agent.URL+"/chat")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader("{}")))

		if rec.Code != status {
			t.Errorf("status = %d, want %d passed through", rec.Code, status)
		}
		if !strings.Contains(rec.Body.String(), "turn exceeded") {
			t.Errorf("body = %q, want the agent's message", rec.Body.String())
		}
		agent.Close()
	}
}

func TestRejectsNonPost(t *testing.T) {
	h := newTestProxy(t, "http://127.0.0.1:1/chat")

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/api/chat", nil))

		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", method, rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != http.MethodPost {
			t.Errorf("%s: Allow = %q, want POST", method, got)
		}
	}
}

func TestUnreachableAgentIsA502(t *testing.T) {
	// Port 1 on loopback: nothing listens, and the connection is refused
	// immediately rather than hanging.
	h := newTestProxy(t, "http://127.0.0.1:1/chat")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader("{}")))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	// The UI parses one envelope for every failure, whoever produced it.
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v (%q)", err, rec.Body.String())
	}
	if !strings.Contains(body["error"], "agent unreachable") {
		t.Errorf("error = %q, want it to say the agent is unreachable", body["error"])
	}
}

func TestSlowAgentIsA504(t *testing.T) {
	release := make(chan struct{})
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer func() {
		close(release)
		agent.Close()
	}()

	h := newTestProxy(t, agent.URL+"/chat", func(o *Options) {
		o.Timeout = 50 * time.Millisecond
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader("{}")))

	if rec.Code != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504", rec.Code)
	}
}

func TestOversizedBodyIsRefused(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain so the cap is enforced on read, which is where MaxBytesReader
		// trips, rather than never being exercised.
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer agent.Close()

	h := newTestProxy(t, agent.URL+"/chat", func(o *Options) { o.MaxBodyBytes = 16 })

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/chat",
		strings.NewReader(strings.Repeat("x", 1024))))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
}

func TestNewRejectsBadTarget(t *testing.T) {
	if _, err := New(Options{Target: "://nope", Log: quietLog()}); err == nil {
		t.Error("New() accepted an unparseable target")
	}
}
