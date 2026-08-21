// Command fathom-front serves Fathom's chat UI and proxies it to fathom-agent.
//
// It holds no Kubernetes credentials and no conversation state. The transcript
// lives in the browser because the agent's /chat is stateless by design, and
// every cluster read still goes through fathom-mcp -- this service only moves
// JSON between a page and the agent.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/spellbound/fathom/fathom-front/internal/config"
	"github.com/spellbound/fathom/fathom-front/internal/proxy"
	"github.com/spellbound/fathom/fathom-front/internal/telemetry"
	"github.com/spellbound/fathom/fathom-front/internal/web"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fathom-front: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := slog.New(telemetry.NewSlogHandler(
		slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel})))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	shutdownTelemetry, err := telemetry.Setup(ctx, cfg.Telemetry, "fathom-front", version, log)
	if err != nil {
		return err
	}
	defer func() {
		// Fresh context: ctx is already cancelled by the time this runs, and a
		// flush needs a live deadline to reach the collector.
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTelemetry(flushCtx); err != nil {
			log.Warn("telemetry shutdown", "error", err)
		}
	}()

	chat, err := proxy.New(proxy.Options{
		Target:       cfg.ChatURL(),
		Timeout:      cfg.AgentTimeout,
		MaxBodyBytes: cfg.MaxBodyBytes,
		Trace:        cfg.Telemetry != telemetry.ModeOff,
		Log:          log,
	})
	if err != nil {
		return err
	}

	ui, err := web.Handler()
	if err != nil {
		return err
	}

	log.Info("starting",
		"version", version,
		"addr", cfg.Addr,
		"agent_url", cfg.AgentURL,
		"agent_timeout", cfg.AgentTimeout)

	return serve(ctx, cfg, chat, ui, log)
}

func serve(ctx context.Context, cfg *config.Config, chat, ui http.Handler, log *slog.Logger) error {
	mux := http.NewServeMux()
	mux.Handle("/api/chat", chat)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/readyz", readyHandler(cfg, log))
	mux.Handle("/", ui)

	httpSrv := &http.Server{
		Addr: cfg.Addr,
		Handler: otelhttp.NewHandler(mux, "fathom-front",
			otelhttp.WithFilter(telemetry.TraceHTTPRequest),
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
				return r.Method + " " + r.URL.Path
			}),
		),
		ReadHeaderTimeout: 10 * time.Second,
		// Deliberately no WriteTimeout. A turn blocks for as long as the model
		// takes -- up to FATHOM_AGENT_REQUEST_TIMEOUT, 180s by default -- and a
		// write deadline would cut the connection mid-answer, turning a slow
		// success into an unexplained failure.
		IdleTimeout: 120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	}
}

// readyHandler reports the agent's readiness as this service's own.
//
// Serving a chat page that cannot reach the agent is worse than not serving
// it: the failure would surface one question and forty seconds later, instead
// of at the probe. The agent's /readyz already caches for 30s and already
// names which dependency failed, so this forwards its body verbatim rather
// than inventing a second, vaguer sentence.
func readyHandler(cfg *config.Config, log *slog.Logger) http.HandlerFunc {
	client := &http.Client{Timeout: cfg.ReadyTimeout}

	return func(w http.ResponseWriter, r *http.Request) {
		// Not instrumented, and not traced -- see telemetry.TraceHTTPRequest.
		// The outbound call inherits no span because the inbound probe has
		// none, so it would start a root trace of its own on every tick.
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, cfg.ReadyURL(), nil)
		if err != nil {
			fail(w, fmt.Sprintf("agent readiness request: %v", err))
			return
		}

		resp, err := client.Do(req)
		if err != nil {
			log.WarnContext(r.Context(), "readiness check failed", "agent_url", cfg.AgentURL, "error", err)
			fail(w, fmt.Sprintf("agent unreachable at %s: %v", cfg.AgentURL, err))
			return
		}
		defer resp.Body.Close()

		// Bounded: /readyz answers in a sentence, and this is echoed to every
		// browser polling the status line.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

		if resp.StatusCode != http.StatusOK {
			log.WarnContext(r.Context(), "agent not ready", "status", resp.StatusCode)
			fail(w, fmt.Sprintf("agent not ready (%d): %s", resp.StatusCode, body))
			return
		}

		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	}
}

func fail(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprintln(w, msg)
}
