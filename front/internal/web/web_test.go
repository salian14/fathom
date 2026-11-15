package web

import (
	"io/fs"
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

	for _, path := range []string{"/app.js", "/evals.js", "/styles.css"} {
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
//
// Walks every .js under static/ rather than naming one file. Naming one meant
// that adding a second script -- which is exactly what the eval view did --
// silently shipped it unguarded.
func TestNoHTMLStringSinks(t *testing.T) {
	scripts, err := fs.Glob(content, "static/*.js")
	if err != nil {
		t.Fatalf("globbing scripts: %v", err)
	}
	if len(scripts) < 2 {
		t.Fatalf("found %d scripts, expected at least app.js and evals.js: %v", len(scripts), scripts)
	}

	for _, name := range scripts {
		js, err := content.ReadFile(name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
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
				t.Errorf("%s uses %s; model output must be rendered as text", name, sink)
			}
		}
	}
}

// TestIndexLoadsScriptsInOrder pins a dependency nothing else can see.
//
// evals.js calls app.js's rendering helpers, and both are classic scripts
// sharing one global scope. Reversing the two script tags fails at runtime with
// a bare ReferenceError, in the browser, with no build step or linter between
// the mistake and the user.
func TestIndexLoadsScriptsInOrder(t *testing.T) {
	index, err := content.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading index.html: %v", err)
	}

	app := strings.Index(string(index), `src="app.js"`)
	evals := strings.Index(string(index), `src="evals.js"`)
	if app < 0 || evals < 0 {
		t.Fatalf("index.html must load both scripts: app.js at %d, evals.js at %d", app, evals)
	}
	if app > evals {
		t.Error("evals.js is loaded before app.js; it depends on app.js's helpers")
	}
}

// TestScriptsRequestRelativeURLs pins the property the lab deployment rests on.
//
// The page is served under /fathom/ behind a gateway shared with other tenants,
// with the prefix stripped before the request reaches this binary. A fetch of
// '/api/chat' would resolve against the origin, leave that prefix, and hit
// whatever route -- or 404 -- lives at the gateway's root. Nothing on a
// port-forward or in run-local would show it, because there the two coincide.
func TestScriptsRequestRelativeURLs(t *testing.T) {
	scripts, err := fs.Glob(content, "static/*.js")
	if err != nil {
		t.Fatalf("globbing scripts: %v", err)
	}

	for _, name := range scripts {
		js, err := content.ReadFile(name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		for i, line := range strings.Split(string(js), "\n") {
			for _, rooted := range []string{"fetch('/", `fetch("/`, "fetch(`/", "_BASE = '/"} {
				if strings.Contains(line, rooted) {
					t.Errorf("%s:%d requests a root-absolute URL (%s); it must be relative to work under the gateway prefix", name, i+1, rooted)
				}
			}
		}
	}
}
