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

// The tests below cover the second shape this package serves: a GET subtree with
// query parameters, for /api/eval. Everything above pins the chat shape, and the
// point of the Options zero value is that those tests did not have to change.

func TestDefaultsToPostOnly(t *testing.T) {
	// Guards the *intent* of TestRejectsNonPost against a future default flip:
	// an Options with nothing set must still be the chat shape.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	p := newTestProxy(t, upstream.URL+"/chat")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/chat", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET on a default proxy: got %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "POST" {
		t.Errorf("Allow = %q, want POST", got)
	}
}

func TestPrefixIsStripped(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	p := newTestProxy(t, upstream.URL, func(o *Options) {
		o.StripPrefix = "/api/eval"
		o.Methods = []string{http.MethodGet}
	})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/eval/runs/7", nil))

	if gotPath != "/runs/7" {
		t.Errorf("upstream path = %q, want /runs/7", gotPath)
	}
}

func TestQueryIsForwardedOnlyWhenAsked(t *testing.T) {
	var gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	t.Run("chat still drops it", func(t *testing.T) {
		// Guards the comment in Rewrite: /chat takes no parameters, and
		// forwarding one would be a silent way to reach whatever it grows later.
		p := newTestProxy(t, upstream.URL+"/chat")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/chat?debug=1", nil))
		if gotQuery != "" {
			t.Errorf("query = %q, want it dropped", gotQuery)
		}
	})

	t.Run("eval forwards it", func(t *testing.T) {
		p := newTestProxy(t, upstream.URL, func(o *Options) {
			o.StripPrefix = "/api/eval"
			o.Methods = []string{http.MethodGet}
			o.PassQuery = true
		})
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/eval/runs?limit=5", nil))
		if gotQuery != "limit=5" {
			t.Errorf("query = %q, want limit=5", gotQuery)
		}
	})
}

func TestRejectsPathOutsideThePrefix(t *testing.T) {
	var reached int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	p := newTestProxy(t, upstream.URL, func(o *Options) {
		o.StripPrefix = "/api/eval"
		o.Methods = []string{http.MethodGet}
	})

	for _, path := range []string{"/api/other/runs", "/api/eval/../../secret"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://example.test", nil)
		req.URL.Path = path // set directly: NewRequest would clean the traversal away
		p.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", path, rec.Code)
		}
	}
	// The assertion that matters: a refused path must never have been forwarded.
	if reached != 0 {
		t.Errorf("upstream was reached %d times; it must never be", reached)
	}
}

func TestServiceNameAppearsInFailures(t *testing.T) {
	p := newTestProxy(t, "http://127.0.0.1:1", func(o *Options) {
		o.StripPrefix = "/api/eval"
		o.Methods = []string{http.MethodGet}
		o.Name = "eval service"
	})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/eval/runs", nil))

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// "agent unreachable" would be a lie on this hop.
	if !strings.Contains(body["error"], "eval service unreachable") {
		t.Errorf("error = %q, want it to name the eval service", body["error"])
	}
}
