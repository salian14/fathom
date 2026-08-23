// Package proxy forwards the browser's /api/* to a service inside the cluster.
//
// The hop exists so the page and the API share an origin. fathom-agent has no
// CORS middleware, and giving it one would mean the agent carrying a list of
// browser origins it otherwise knows nothing about; proxying keeps that
// knowledge here, where the browser already is.
//
// Nothing is translated. The request body, the response body and the status
// code pass through untouched, so the agent's own error vocabulary -- 400 for a
// bad history role, 422 for an empty question, 502 for a failed dependency, 504
// for an exceeded turn -- is what the UI sees and reports.
//
// Two shapes share this code. /api/chat is one POST to one fixed path with no
// query string; /api/eval/* is a GET subtree with parameters. The Options zero
// value is the chat shape, deliberately: the fields that turn on the subtree
// behaviour all default off, so nothing about the chat hop changed when the
// second one arrived.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Options configures New. Everything here comes from config.Config.
type Options struct {
	// Target is the agent's full chat endpoint, e.g.
	// "http://fathom-agent.fathom.svc.cluster.local:8080/chat".
	Target string

	// Timeout bounds one proxied turn. It must exceed the agent's own request
	// timeout so the agent answers first; see config.Config.AgentTimeout.
	Timeout time.Duration

	// MaxBodyBytes caps the inbound request body.
	MaxBodyBytes int64

	// Trace turns on the otelhttp transport, which is what puts traceparent on
	// the outbound request and so joins this span to the agent's.
	Trace bool

	// Methods this proxy accepts. Empty means POST only -- the chat behaviour,
	// and what TestRejectsNonPost pins.
	Methods []string

	// StripPrefix is removed from the inbound path before what remains is
	// joined to Target's path. Empty discards the inbound path entirely and
	// uses Target's verbatim, which is what confines /api/chat to exactly
	// /chat and nothing else reachable through it.
	StripPrefix string

	// PassQuery forwards the inbound query string. Off by default: /chat takes
	// no parameters, and forwarding one would be a silent way to reach
	// whatever the agent might grow later.
	PassQuery bool

	// Name is the service named in transport-failure messages -- "agent",
	// "eval service". Empty means "agent", because that was the only caller
	// when these messages were written.
	Name string

	Log *slog.Logger
}

// New returns a handler for one proxied route.
func New(opts Options) (http.Handler, error) {
	target, err := url.Parse(opts.Target)
	if err != nil {
		return nil, err
	}

	// Precomputed once rather than per request.
	allowed, allow := methodSet(opts.Methods)
	prefix := strings.TrimSuffix(opts.StripPrefix, "/")
	name := opts.Name
	if name == "" {
		name = "agent"
	}

	// http.DefaultTransport is cloned rather than shared: ResponseHeaderTimeout
	// below would otherwise apply to every other client in the process.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// The agent sends nothing until the whole turn is done -- it awaits
	// graph.ainvoke and then writes one JSON body -- so "time to first response
	// header" is the turn's wall time, and this is the timeout that matters.
	// A plain client Timeout would do as well here, but this one names the
	// thing being waited on.
	transport.ResponseHeaderTimeout = opts.Timeout

	var rt http.RoundTripper = transport
	if opts.Trace {
		rt = otelhttp.NewTransport(transport)
	}

	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			if prefix == "" {
				// SetURL joins paths; the target already names /chat exactly and
				// the inbound path (/api/chat) must not be appended to it.
				r.Out.URL.Path = target.Path
			} else {
				rest := strings.TrimPrefix(r.In.URL.Path, prefix)
				r.Out.URL.Path = strings.TrimSuffix(target.Path, "/") + "/" + strings.TrimPrefix(rest, "/")
			}
			if opts.PassQuery {
				r.Out.URL.RawQuery = r.In.URL.RawQuery
			} else {
				r.Out.URL.RawQuery = ""
			}
			r.Out.Host = target.Host
			// No X-Forwarded-For: the agent does not use it, and forwarding a
			// client IP into a service that logs freely is a privacy cost with
			// no operational return.
		},
		Transport: rt,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// The agent's own failures arrive as real responses and never land
			// here. This is the transport failing: refused, timed out, or the
			// client hanging up mid-turn.
			status, msg := classify(err, name)
			opts.Log.WarnContext(r.Context(), "proxy failed",
				"service", name, "target", opts.Target, "status", status, "error", err)
			writeError(w, status, msg)
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[r.Method] {
			w.Header().Set("Allow", allow)
			writeError(w, http.StatusMethodNotAllowed, "only "+allow+" is accepted here")
			return
		}
		// A path outside the prefix, or one carrying a traversal segment, is
		// refused here rather than rewritten into something upstream. ServeMux
		// already cleans paths before we see them, but this is the one function
		// in this repo where a mistake turns the only externally-reachable pod
		// into a relay into the cluster network, so it does not lean on that.
		if prefix != "" {
			if !strings.HasPrefix(r.URL.Path, prefix) || strings.Contains(r.URL.Path, "..") {
				writeError(w, http.StatusBadRequest, "path outside "+prefix)
				return
			}
		}
		// Bounded before forwarding, not after: an unbounded body would
		// otherwise be streamed to the agent in full before anything checked it.
		r.Body = http.MaxBytesReader(w, r.Body, opts.MaxBodyBytes)
		rp.ServeHTTP(w, r)
	}), nil
}

// classify turns a transport error into the status and sentence the UI shows.
// The distinction that matters to whoever is reading it: "the agent is not
// there" is a different afternoon from "the agent is thinking too slowly".
func classify(err error, name string) (int, string) {
	var maxBytes *http.MaxBytesError
	switch {
	case errors.As(err, &maxBytes):
		return http.StatusRequestEntityTooLarge,
			"request too large -- start a new chat, the transcript has outgrown the limit"
	case errors.Is(err, context.DeadlineExceeded), os.IsTimeout(err):
		return http.StatusGatewayTimeout, "the " + name + " did not answer in time"
	case errors.Is(err, context.Canceled):
		// The browser aborted (Stop, navigation, reload). Nothing is wrong and
		// nobody is listening, but ReverseProxy still wants a status.
		return 499, "request cancelled by the client"
	default:
		return http.StatusBadGateway, name + " unreachable: " + err.Error()
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The same {"error": ...} envelope fathom-agent uses for 400/502/504, so
	// the UI has one shape to parse rather than one per failure origin.
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// methodSet returns the lookup and the Allow header for the configured methods,
// defaulting to POST -- the chat shape, and the only one until /api/eval.
func methodSet(methods []string) (map[string]bool, string) {
	if len(methods) == 0 {
		methods = []string{http.MethodPost}
	}
	set := make(map[string]bool, len(methods))
	for _, m := range methods {
		set[strings.ToUpper(m)] = true
	}
	return set, strings.Join(methods, ", ")
}
