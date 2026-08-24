package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"k8s.io/klog/v2"

	"github.com/spellbound/fathom/mcp/internal/config"
	"github.com/spellbound/fathom/mcp/internal/kube"
	"github.com/spellbound/fathom/mcp/internal/prom"
	"github.com/spellbound/fathom/mcp/internal/telemetry"
	"github.com/spellbound/fathom/mcp/internal/tools"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fathom-mcp: %v\n", err)
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
	klog.SetSlogLogger(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Set up before the transport branch below: installing this inside
	// serveHTTP would leave the stdio path -- the one used when debugging a
	// tool locally -- with no traces at all.
	shutdownTelemetry, err := telemetry.Setup(ctx, cfg.Telemetry, "fathom-mcp", version, log)
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

	client, err := kube.New(kube.Options{
		KubeconfigPath: cfg.KubeconfigPath,
		UserAgent:      "fathom-mcp/" + version,
		QPS:            20,
		Burst:          40,
		Trace:          cfg.Telemetry != "off",
	})
	if err != nil {
		return err
	}

	// Built only when a URL is configured. A nil client is the signal that the
	// metrics tools are not registered, so nothing downstream has to check.
	var promClient *prom.Client
	if cfg.MetricsEnabled() {
		promClient, err = prom.New(prom.Options{
			BaseURL:   cfg.PrometheusURL,
			Timeout:   cfg.PrometheusTimeout,
			UserAgent: "fathom-mcp/" + version,
			Trace:     cfg.Telemetry != "off",
		})
		if err != nil {
			return err
		}
	}

	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "fathom-mcp",
		Version: version,
	}, &mcp.ServerOptions{
		Instructions: instructions(cfg.MetricsEnabled()),
	})
	tools.New(cfg, client, promClient, log).Register(srv)

	// Protocol-level spans, which is where the agent's traceparent is actually
	// reachable -- see telemetry.Receiving. Registered for both transports.
	mw, err := telemetry.ReceivingMiddleware()
	if err != nil {
		return err
	}
	srv.AddReceivingMiddleware(mw)

	// prometheus_url is logged even when empty: "the metrics tools are missing"
	// is otherwise diagnosed by elimination, and this turns it into line one.
	log.Info("starting",
		"version", version,
		"transport", cfg.Transport,
		"allowed_resources", cfg.AllowedResources,
		"allowed_namespaces", cfg.AllowedNamespaces,
		"prometheus_url", cfg.PrometheusURL,
		"metrics_tools", cfg.MetricsEnabled())

	if cfg.Transport == "stdio" {
		return srv.Run(ctx, &mcp.StdioTransport{})
	}
	return serveHTTP(ctx, cfg, srv, client, log)
}

// instructions is the server's own preamble, which the model reads before any
// tool description. It has to match what is actually registered -- describing
// metrics tools on a deployment that has none would send the model looking for
// tools that are not there.
func instructions(metrics bool) string {
	base := "Read-only access to a Kubernetes cluster. Survey with k8s_get, " +
		"diagnose a specific object with k8s_describe, and read container output with " +
		"k8s_log."
	if metrics {
		base += " For how the cluster is behaving over time rather than how it is " +
			"configured, use list_metrics to find a metric, describe_metric to learn its " +
			"labels, then run_promql to query it."
	}
	return base + " Nothing here can change the cluster."
}

func serveHTTP(ctx context.Context, cfg *config.Config, srv *mcp.Server, client *kube.Client, log *slog.Logger) error {
	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{
			Logger:         log,
			SessionTimeout: 10 * time.Minute,
		},
	)

	mux := http.NewServeMux()
	mux.Handle(cfg.MCPPath, handler)
	mux.Handle(cfg.MCPPath+"/", handler)

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if err := client.Ping(); err != nil {
			log.Warn("readiness check failed", "error", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, "kubernetes api unreachable: %v\n", err)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	httpSrv := &http.Server{
		Addr: cfg.Addr,
		// Probes and unparented traffic are filtered out rather than sampled
		// away -- see telemetry.TraceHTTPRequest for why both qualify.
		Handler: otelhttp.NewHandler(mux, "fathom-mcp",
			otelhttp.WithFilter(telemetry.TraceHTTPRequest),
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
				return r.Method + " " + r.URL.Path
			}),
		),
		ReadHeaderTimeout: 10 * time.Second,
		// Deliberately no WriteTimeout: responses here are SSE streams.
		IdleTimeout: 120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr, "path", cfg.MCPPath)
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
