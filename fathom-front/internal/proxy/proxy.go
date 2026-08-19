// Package proxy forwards the browser's /api/chat to fathom-agent's /chat.
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

	Log *slog.Logger
}

// New returns a handler for POST /api/chat.
func New(opts Options) (http.Handler, error) {
	target, err := url.Parse(opts.Target)
	if err != nil {
		return nil, err
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
			// SetURL joins paths; the target already names /chat exactly and
			// the inbound path (/api/chat) must not be appended to it.
			r.Out.URL.Path = target.Path
			r.Out.URL.RawQuery = ""
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
			status, msg := classify(err)
			opts.Log.WarnContext(r.Context(), "proxy to agent failed",
				"target", opts.Target, "status", status, "error", err)
			writeError(w, status, msg)
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeError(w, http.StatusMethodNotAllowed, "only POST is accepted here")
			return
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
func classify(err error) (int, string) {
	var maxBytes *http.MaxBytesError
	switch {
	case errors.As(err, &maxBytes):
		return http.StatusRequestEntityTooLarge,
			"request too large -- start a new chat, the transcript has outgrown the limit"
	case errors.Is(err, context.DeadlineExceeded), os.IsTimeout(err):
		return http.StatusGatewayTimeout, "the agent did not answer in time"
	case errors.Is(err, context.Canceled):
		// The browser aborted (Stop, navigation, reload). Nothing is wrong and
		// nobody is listening, but ReverseProxy still wants a status.
		return 499, "request cancelled by the client"
	default:
		return http.StatusBadGateway, "agent unreachable: " + err.Error()
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The same {"error": ...} envelope fathom-agent uses for 400/502/504, so
	// the UI has one shape to parse rather than one per failure origin.
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
