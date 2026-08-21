// Package telemetry wires OpenTelemetry into fathom-front: OTLP traces and
// metrics over gRPC, plus a slog handler that correlates logs to spans.
//
// This is a deliberately trimmed port of mcp/internal/telemetry. The two live
// in separate Go modules with separate release lifecycles, so there is no
// import path between them; what is shared is the contract (the three modes,
// the OTEL_* variables, the failure posture), not the code. The MCP middleware
// and the Kubernetes discovery filter have no analogue here and are not copied.
//
// Two rules govern everything here, unchanged from fathom-mcp:
//
//   - Telemetry never takes the workload down. The collector lives in another
//     namespace and may be absent, unreachable, or NetworkPolicy'd off. Every
//     failure path degrades to no-op providers and a warning.
//   - The propagator is installed unconditionally, even when exporting is off.
//     A service that emits nothing should still pass trace context through to
//     the next hop -- which for this service is the whole point, since it is
//     where a browser request first acquires a trace id.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// Modes accepted by FATHOM_FRONT_TELEMETRY.
const (
	ModeOTLP    = "otlp"    // export to the collector named by OTEL_EXPORTER_OTLP_ENDPOINT
	ModeConsole = "console" // print to stderr; no collector needed
	ModeOff     = "off"     // propagate context, export nothing
)

// Shutdown flushes and stops the providers installed by Setup.
type Shutdown func(context.Context) error

// Setup installs the global tracer provider, meter provider and propagator.
//
// It returns a usable Shutdown and a nil error even when the exporter cannot be
// built: a collector that is down must not stop the UI from being served.
func Setup(ctx context.Context, mode, service, version string, log *slog.Logger) (Shutdown, error) {
	// Unconditional, and deliberately before the mode check: Go's global
	// propagator is a no-op until this is called, so skipping it when telemetry
	// is off would silently break propagation into fathom-agent.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		log.Warn("opentelemetry", "error", err)
	}))

	if mode == ModeOff {
		return func(context.Context) error { return nil }, nil
	}

	res, err := resource.Merge(
		resource.Default(), // reads OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(service),
			semconv.ServiceVersion(version),
		),
	)
	if err != nil {
		// A schema-URL conflict is not worth failing a deploy over.
		log.Warn("opentelemetry resource merge, falling back to defaults", "error", err)
		res = resource.Default()
	}

	spanExp, metricExp, err := exporters(ctx, mode)
	if err != nil {
		log.Warn("opentelemetry exporters unavailable, telemetry disabled", "error", err)
		return func(context.Context) error { return nil }, nil
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(spanExp),
		sdktrace.WithResource(res),
	)
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp,
			sdkmetric.WithProducer(runtime.NewProducer()))),
	)

	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)

	if err := runtime.Start(runtime.WithMinimumReadMemStatsInterval(15 * time.Second)); err != nil {
		log.Warn("opentelemetry runtime metrics", "error", err)
	}

	log.Info("telemetry configured", "mode", mode, "service", service)

	return func(ctx context.Context) error {
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
	}, nil
}

func exporters(ctx context.Context, mode string) (sdktrace.SpanExporter, sdkmetric.Exporter, error) {
	if mode == ModeConsole {
		// stderr, matching the other two components. Nothing here writes a
		// protocol to stdout, but keeping one answer to "where does console
		// telemetry go" across all three is worth more than the exception.
		spanExp, err := stdouttrace.New(stdouttrace.WithWriter(os.Stderr))
		if err != nil {
			return nil, nil, fmt.Errorf("console trace exporter: %w", err)
		}
		metricExp, err := stdoutmetric.New(stdoutmetric.WithWriter(os.Stderr))
		if err != nil {
			return nil, nil, fmt.Errorf("console metric exporter: %w", err)
		}
		return spanExp, metricExp, nil
	}

	// WithInsecure is explicit rather than inferred from the endpoint's http://
	// scheme, so a bare "host:4317" works too. Neither call dials here;
	// connection is lazy, which keeps a missing collector out of startup.
	spanExp, err := otlptracegrpc.New(ctx, otlptracegrpc.WithInsecure())
	if err != nil {
		return nil, nil, fmt.Errorf("otlp trace exporter: %w", err)
	}
	metricExp, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithInsecure())
	if err != nil {
		return nil, nil, fmt.Errorf("otlp metric exporter: %w", err)
	}
	return spanExp, metricExp, nil
}
