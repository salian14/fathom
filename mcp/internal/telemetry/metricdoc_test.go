package telemetry

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// TestDeclaredAttributesAreWhatIsRecorded is the guard that makes MetricDocs
// safe to publish. The declaration is hand-written -- attribute keys are chosen
// at record time and no provider can enumerate them at startup -- so it is
// worth exactly as much as this test.
//
// It drives a real tool call through the real middleware and reads back what
// the SDK actually saw, so what is asserted is the emitted stream rather than a
// restatement of it. That also enforces the rule the declaration exists to make
// visible: an unbounded key added here (a session id, an object name) fails
// this test instead of becoming a cardinality incident weeks later.
func TestDeclaredAttributesAreWhatIsRecorded(t *testing.T) {
	const metricName = "mcp.server.operation.duration"

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	duration, err := provider.Meter(scopeName).Float64Histogram(metricName)
	if err != nil {
		t.Fatalf("histogram: %v", err)
	}

	_, tracer := recorder(t)
	session := connect(t, Receiving(tracer, duration))
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "k8s_get", Arguments: map[string]any{},
	}); err != nil {
		t.Fatal(err)
	}

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("collect: %v", err)
	}

	observed := map[string]bool{}
	found := false
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != metricName {
				continue
			}
			found = true
			hist, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("unexpected data type %T", m.Data)
			}
			for _, dp := range hist.DataPoints {
				for _, kv := range dp.Attributes.ToSlice() {
					observed[string(kv.Key)] = true
				}
			}
		}
	}
	if !found {
		t.Fatalf("%s was never recorded; the middleware no longer feeds it", metricName)
	}

	declared := map[string]bool{}
	for _, d := range MetricDocs() {
		if d.Name == metricName {
			for _, a := range d.Attributes {
				declared[a] = true
			}
		}
	}

	for key := range observed {
		if !declared[key] {
			t.Errorf("attribute %q is recorded but undeclared -- either declare it in "+
				"MetricDocs or, if it is unbounded, keep it on the span instead", key)
		}
	}
	for key := range declared {
		if !observed[key] {
			t.Errorf("attribute %q is declared in MetricDocs but never recorded", key)
		}
	}
}

// TestEveryDeclaredMetricIsDescribed keeps the published document usable: a
// name with no unit or description is not a contract, it is a reminder.
func TestEveryDeclaredMetricIsDescribed(t *testing.T) {
	for _, d := range MetricDocs() {
		if d.Name == "" || d.Kind == "" || d.Unit == "" || d.Description == "" {
			t.Errorf("incomplete MetricDoc: %+v", d)
		}
	}
}
