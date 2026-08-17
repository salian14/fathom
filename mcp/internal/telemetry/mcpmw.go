package telemetry

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Attribute keys for MCP, following the shape the MCP semantic conventions are
// converging on. Kept as constants because they are also metric dimensions and
// a typo would silently split a time series.
const (
	attrMethod    = attribute.Key("mcp.method.name")
	attrTool      = attribute.Key("mcp.tool.name")
	attrSessionID = attribute.Key("mcp.session.id")
)

// ReceivingMiddleware builds Receiving from the global providers. Call it
// after Setup; with telemetry off the globals are no-ops and the middleware
// costs a map lookup per call.
func ReceivingMiddleware() (mcp.Middleware, error) {
	meter := otel.Meter(scopeName)
	duration, err := meter.Float64Histogram("mcp.server.operation.duration",
		metric.WithDescription("Duration of an inbound MCP method call."),
		metric.WithUnit("s"))
	if err != nil {
		return nil, err
	}
	return Receiving(otel.Tracer(scopeName), duration), nil
}

// scopeName is the instrumentation scope reported for every span and metric
// this package produces.
const scopeName = "github.com/spellbound/fathom/mcp"

// Receiving returns middleware that opens a span per inbound JSON-RPC method.
//
// This is not a nicety on top of the HTTP handler span -- it is the only thing
// that connects the agent's trace to this server's work. The handler context
// comes off the jsonrpc2 connection loop rather than the HTTP request that
// delivered the message, so an otelhttp server span on the mux is a dead end
// that nothing downstream inherits. The per-request headers reachable through
// Extra are where the agent's traceparent actually lives.
//
// It also works under stdio, where there is no HTTP layer at all: Extra is nil
// there, and every call correctly becomes a root span.
func Receiving(tracer trace.Tracer, duration metric.Float64Histogram) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			// ping is the protocol's keepalive. Tracing it buys nothing and
			// would put a span on the timeline every few seconds forever.
			if method == methodPing {
				return next(ctx, method, req)
			}

			if extra := req.GetExtra(); extra != nil && extra.Header != nil {
				ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.HeaderCarrier(extra.Header))
			}

			name, attrs := describeCall(method, req)

			ctx, span := tracer.Start(ctx, name,
				trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(attrs...))
			defer span.End()

			started := time.Now()
			result, err := next(ctx, method, req)

			outcome := "ok"
			if err != nil {
				outcome = "error"
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
			}

			if duration != nil {
				// Only bounded attributes here. Method names come from the
				// protocol and tool names from a three-entry registry; the
				// session id and any object name stay on the span.
				duration.Record(ctx, time.Since(started).Seconds(),
					metric.WithAttributes(
						attrMethod.String(method),
						attrTool.String(toolName(req)),
						attribute.String("outcome", outcome),
					))
			}

			return result, err
		}
	}
}

const (
	methodPing     = "ping"
	methodToolCall = "tools/call"
)

// describeCall names the span and collects its attributes. "tools/call k8s_get"
// reads better on a timeline than a bare "tools/call" repeated three times.
func describeCall(method string, req mcp.Request) (string, []attribute.KeyValue) {
	attrs := []attribute.KeyValue{attrMethod.String(method)}

	// Type-asserted rather than nil-checked as an interface: GetSession returns
	// a non-nil interface wrapping a nil *ServerSession when a request was not
	// delivered over a live session, and calling ID() on that panics.
	if session, ok := req.GetSession().(*mcp.ServerSession); ok && session != nil {
		if id := session.ID(); id != "" {
			attrs = append(attrs, attrSessionID.String(id))
		}
	}

	name := method
	if tool := toolName(req); tool != "" {
		attrs = append(attrs, attrTool.String(tool))
		name = method + " " + tool
	}
	return name, attrs
}

// toolName returns the tool being invoked, or "" for any other method.
func toolName(req mcp.Request) string {
	params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
	if !ok || params == nil {
		return ""
	}
	return params.Name
}
