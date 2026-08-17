package telemetry

import (
	"context"
	"net/http"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// recorder installs a span recorder and returns it with a tracer drawn from
// the same provider.
func recorder(t *testing.T) (*tracetest.SpanRecorder, trace.Tracer) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return sr, tp.Tracer("test")
}

// connect wires a server with the given middleware to an in-memory client.
//
// In-memory transports have no HTTP layer at all, which is exactly the point:
// what passes here is what works under the stdio transport.
func connect(t *testing.T, mw mcp.Middleware) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()

	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "v0"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "k8s_get", Description: "test"},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
		})
	srv.AddReceivingMiddleware(mw)

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil).
		Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestReceivingNamesToolCalls(t *testing.T) {
	sr, tracer := recorder(t)
	session := connect(t, Receiving(tracer, nil))

	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "k8s_get", Arguments: map[string]any{},
	}); err != nil {
		t.Fatal(err)
	}

	names := map[string]bool{}
	for _, span := range sr.Ended() {
		names[span.Name()] = true
	}
	if !names["tools/call k8s_get"] {
		t.Errorf("no span named %q; got %v", "tools/call k8s_get", names)
	}
}

func TestReceivingSetsToolAttribute(t *testing.T) {
	sr, tracer := recorder(t)
	session := connect(t, Receiving(tracer, nil))

	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "k8s_get", Arguments: map[string]any{},
	}); err != nil {
		t.Fatal(err)
	}

	for _, span := range sr.Ended() {
		if span.Name() != "tools/call k8s_get" {
			continue
		}
		for _, attr := range span.Attributes() {
			if attr.Key == attrTool && attr.Value.AsString() == "k8s_get" {
				return
			}
		}
		t.Fatalf("span %q has no %s attribute: %v", span.Name(), attrTool, span.Attributes())
	}
	t.Fatal("tool span not recorded")
}

// ping is the protocol keepalive: tracing it would put a span on the timeline
// every few seconds for the life of the session.
func TestReceivingSkipsPing(t *testing.T) {
	sr, tracer := recorder(t)
	session := connect(t, Receiving(tracer, nil))

	if err := session.Ping(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	for _, span := range sr.Ended() {
		if span.Name() == "ping" {
			t.Error("ping produced a span")
		}
	}
}

// The contract that makes a trace span both services: the agent's traceparent
// arrives on the HTTP headers of the POST, reachable only through Extra --
// the handler context comes off the jsonrpc2 connection loop, not the request.
func TestReceivingExtractsTraceparentFromHeader(t *testing.T) {
	sr, tracer := recorder(t)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	const (
		traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
		spanID  = "00f067aa0ba902b7"
	)
	header := http.Header{}
	header.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")

	mw := Receiving(tracer, nil)
	handler := mw(func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return &mcp.CallToolResult{}, nil
	})

	req := &mcp.ServerRequest[*mcp.CallToolParamsRaw]{
		Params: &mcp.CallToolParamsRaw{Name: "k8s_get"},
		Extra:  &mcp.RequestExtra{Header: header},
	}
	if _, err := handler(context.Background(), "tools/call", req); err != nil {
		t.Fatal(err)
	}

	ended := sr.Ended()
	if len(ended) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(ended))
	}
	if got := ended[0].SpanContext().TraceID().String(); got != traceID {
		t.Errorf("trace id = %s, want %s (the agent's traceparent was not adopted)", got, traceID)
	}
	if got := ended[0].Parent().SpanID().String(); got != spanID {
		t.Errorf("parent span id = %s, want %s", got, spanID)
	}
}

// The readiness probe opens a full MCP session with instrumentation
// suppressed, so its requests arrive over HTTP with no trace context. Tracing
// that session costs eight spans every time the probe runs.
func TestReceivingSkipsUnparentedSessionLifecycle(t *testing.T) {
	sr, tracer := recorder(t)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	handler := Receiving(tracer, nil)(func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return &mcp.CallToolResult{}, nil
	})

	for _, method := range []string{"initialize", "notifications/initialized", "tools/list"} {
		req := &mcp.ServerRequest[*mcp.CallToolParamsRaw]{
			Params: &mcp.CallToolParamsRaw{},
			Extra:  &mcp.RequestExtra{Header: http.Header{}}, // over HTTP, unparented
		}
		if _, err := handler(context.Background(), method, req); err != nil {
			t.Fatal(err)
		}
	}

	if got := len(sr.Ended()); got != 0 {
		t.Errorf("probe session produced %d spans, want 0", got)
	}
}

// The exemption that keeps the previous test honest: if propagation ever
// breaks, real work must surface as root spans rather than go silent.
func TestReceivingTracesUnparentedToolCalls(t *testing.T) {
	sr, tracer := recorder(t)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	handler := Receiving(tracer, nil)(func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return &mcp.CallToolResult{}, nil
	})

	req := &mcp.ServerRequest[*mcp.CallToolParamsRaw]{
		Params: &mcp.CallToolParamsRaw{Name: "k8s_get"},
		Extra:  &mcp.RequestExtra{Header: http.Header{}},
	}
	if _, err := handler(context.Background(), "tools/call", req); err != nil {
		t.Fatal(err)
	}

	ended := sr.Ended()
	if len(ended) != 1 {
		t.Fatalf("recorded %d spans, want 1: a tools/call must never be dropped", len(ended))
	}
	if ended[0].Name() != "tools/call k8s_get" {
		t.Errorf("span name = %q", ended[0].Name())
	}
}

// A parented session is real work, not a probe, and stays traced.
func TestReceivingTracesParentedSessionLifecycle(t *testing.T) {
	sr, tracer := recorder(t)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	header := http.Header{}
	header.Set("traceparent", sampledTraceparent)

	handler := Receiving(tracer, nil)(func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return &mcp.CallToolResult{}, nil
	})

	req := &mcp.ServerRequest[*mcp.CallToolParamsRaw]{
		Params: &mcp.CallToolParamsRaw{},
		Extra:  &mcp.RequestExtra{Header: header},
	}
	if _, err := handler(context.Background(), "initialize", req); err != nil {
		t.Fatal(err)
	}

	if got := len(sr.Ended()); got != 1 {
		t.Errorf("parented initialize produced %d spans, want 1", got)
	}
}

// Under stdio there is no HTTP layer, so Extra is nil. That must yield a clean
// root span rather than a panic -- and the unparented rules must not apply
// there, since stdio has no probe traffic and never carries trace context.
func TestReceivingHandlesMissingExtra(t *testing.T) {
	sr, tracer := recorder(t)

	handler := Receiving(tracer, nil)(func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return &mcp.CallToolResult{}, nil
	})

	req := &mcp.ServerRequest[*mcp.CallToolParamsRaw]{
		Params: &mcp.CallToolParamsRaw{Name: "k8s_get"},
	}
	if _, err := handler(context.Background(), "tools/call", req); err != nil {
		t.Fatal(err)
	}

	ended := sr.Ended()
	if len(ended) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(ended))
	}
	if ended[0].Parent().IsValid() {
		t.Error("want a root span when no header carried trace context")
	}
}
