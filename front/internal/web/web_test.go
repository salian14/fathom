package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServesIndexAtRoot(t *testing.T) {
	h, err := Handler()
	if err != nil {
		t.Fatalf("Handler(): %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<title>Fathom</title>") {
		t.Error("root did not serve index.html")
	}
	// Unversioned assets against a rolling Deployment: a cached app.js held
	// against a new binary is the failure this prevents.
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

func TestServesAssets(t *testing.T) {
	h, err := Handler()
	if err != nil {
		t.Fatalf("Handler(): %v", err)
	}

	for _, path := range []string{"/app.js", "/styles.css"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, rec.Code)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s: empty", path)
		}
	}
}

// The UI is built with createElement and textContent throughout, because the
// answers it renders are model output describing cluster objects whose fields
// are set by whoever created them. One HTML-string sink would undo that, so
// this fails the build rather than trusting the convention to hold.
//
// Comments are stripped first: app.js explains this rule in prose, and a test
// that its own documentation trips is a test people delete.
func TestNoHTMLStringSinks(t *testing.T) {
	js, err := content.ReadFile("static/app.js")
	if err != nil {
		t.Fatalf("reading app.js: %v", err)
	}

	var code strings.Builder
	for _, line := range strings.Split(string(js), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		code.WriteString(line)
		code.WriteByte('\n')
	}

	for _, sink := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write"} {
		if strings.Contains(code.String(), sink) {
			t.Errorf("app.js uses %s; model output must be rendered as text", sink)
		}
	}
}
