package tools

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/spellbound/fathom/mcp/internal/config"
)

// TestErrorMetricAttributesMatchTheDeclaration drives the real failure path and
// compares what the SDK saw against MetricDocs.
//
// This is the counter docs/conventions.md prohibition 4 is about. A label added
// here that is not from a closed set -- an object name, a selector, a PromQL
// query -- multiplies the time series and is the kind of change that looks
// harmless in review. It fails here instead.
func TestErrorMetricAttributesMatchTheDeclaration(t *testing.T) {
	const metricName = "mcp.tool.errors"

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	defer otel.SetMeterProvider(previous)

	// New resolves its counter from the global meter, which is why the provider
	// is installed above rather than passed in.
	s := New(&config.Config{}, nil, nil, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	s.failed(context.Background(), "k8s_get", "pods", errors.New("boom"))

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
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("unexpected data type %T", m.Data)
			}
			for _, dp := range sum.DataPoints {
				for _, kv := range dp.Attributes.ToSlice() {
					observed[string(kv.Key)] = true
				}
			}
		}
	}
	if !found {
		t.Fatalf("%s was never recorded; the failure path no longer feeds it", metricName)
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
			t.Errorf("attribute %q is recorded but undeclared. If it is not a closed "+
				"set, it belongs on the span, not on this counter", key)
		}
	}
	for key := range declared {
		if !observed[key] {
			t.Errorf("attribute %q is declared in MetricDocs but never recorded", key)
		}
	}
}
