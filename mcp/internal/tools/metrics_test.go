package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/spellbound/fathom/mcp/internal/prom"
)

func TestFormatLabels(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{"empty", nil, "{}"},
		{"name only", map[string]string{"__name__": "up"}, "up"},
		{
			// Keys sorted, so the same series renders identically every time
			// and the model can paste the result back into a query.
			"name and labels",
			map[string]string{"__name__": "up", "job": "kubelet", "instance": "10.0.0.1:10250"},
			`up{instance="10.0.0.1:10250",job="kubelet"}`,
		},
		{
			"aggregation drops the name",
			map[string]string{"namespace": "fathom"},
			`{namespace="fathom"}`,
		},
		{
			"values are quoted, so an embedded quote cannot forge a matcher",
			map[string]string{"path": `a"b`},
			`{path="a\"b"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatLabels(tt.labels); got != tt.want {
				t.Errorf("formatLabels() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestMetricSelector(t *testing.T) {
	if got := metricSelector("up", ""); got != `{__name__="up"}` {
		t.Errorf("metricSelector without matchers = %s", got)
	}
	if got := metricSelector("up", `job="kubelet"`); got != `{__name__="up",job="kubelet"}` {
		t.Errorf("metricSelector with matchers = %s", got)
	}
}

func vectorResult(t *testing.T, samples string) *prom.QueryResult {
	t.Helper()
	return &prom.QueryResult{ResultType: prom.ResultVector, Result: json.RawMessage("[" + samples + "]")}
}

func TestRenderQueryResultVector(t *testing.T) {
	result := vectorResult(t,
		`{"metric":{"__name__":"up","job":"kubelet"},"value":[1,"1"]},`+
			`{"metric":{"__name__":"up","job":"api"},"value":[1,"0"]}`)

	out, err := renderQueryResult(result, 100, nil)
	if err != nil {
		t.Fatalf("renderQueryResult: %v", err)
	}
	if !strings.Contains(out, "SERIES") || !strings.Contains(out, "VALUE") {
		t.Errorf("vector rendered without a header:\n%s", out)
	}
	if !strings.Contains(out, `up{job="kubelet"}`) {
		t.Errorf("vector lost a series:\n%s", out)
	}
	if strings.Contains(out, "showing") {
		t.Errorf("an uncapped result claimed to be capped:\n%s", out)
	}
}

// A model that is not told the result was cut will report the missing series
// as absent from the cluster -- the same failure truncate() guards against.
func TestRenderQueryResultCapsSeries(t *testing.T) {
	var samples []string
	for i := 0; i < 20; i++ {
		samples = append(samples, `{"metric":{"__name__":"up"},"value":[1,"1"]}`)
	}
	out, err := renderQueryResult(vectorResult(t, strings.Join(samples, ",")), 5, nil)
	if err != nil {
		t.Fatalf("renderQueryResult: %v", err)
	}

	if strings.Count(out, "up") != 5 {
		t.Errorf("rendered %d series, want 5:\n%s", strings.Count(out, "up"), out)
	}
	if !strings.Contains(out, "showing 5 of 20 series") {
		t.Errorf("capped result does not say so:\n%s", out)
	}
	if !strings.Contains(out, "sum by") {
		t.Errorf("capped result does not say how to see all of it:\n%s", out)
	}
}

func TestRenderQueryResultEmptyVector(t *testing.T) {
	out, err := renderQueryResult(vectorResult(t, ""), 100, nil)
	if err != nil {
		t.Fatalf("renderQueryResult: %v", err)
	}
	if !strings.Contains(out, "matched no series") {
		t.Errorf("an empty result rendered as a bare table:\n%s", out)
	}
}

func TestRenderQueryResultMatrixKeepsTheNewestPoints(t *testing.T) {
	var points []string
	for i := 0; i < maxMatrixPoints+5; i++ {
		points = append(points, `[`+itoa(1755432000+i)+`,"`+itoa(i)+`"]`)
	}
	result := &prom.QueryResult{
		ResultType: prom.ResultMatrix,
		Result: json.RawMessage(`[{"metric":{"__name__":"up"},"values":[` +
			strings.Join(points, ",") + `]}]`),
	}

	out, err := renderQueryResult(result, 100, nil)
	if err != nil {
		t.Fatalf("renderQueryResult: %v", err)
	}
	if !strings.Contains(out, "5 earlier points not shown") {
		t.Errorf("dropped points not announced:\n%s", out)
	}
	// The newest sample is the one that answers "what is it doing now", so it
	// is the end of the series that must survive, not the start.
	if !strings.Contains(out, "Z  "+itoa(maxMatrixPoints+4)+"\n") {
		t.Errorf("matrix dropped the newest point:\n%s", out)
	}
	if strings.Contains(out, "Z  0\n") {
		t.Errorf("matrix kept the oldest point instead of the newest:\n%s", out)
	}
}

func TestRenderQueryResultScalarAndUnknown(t *testing.T) {
	scalar := &prom.QueryResult{ResultType: prom.ResultScalar, Result: json.RawMessage(`[1755432000,"42"]`)}
	out, err := renderQueryResult(scalar, 100, nil)
	if err != nil {
		t.Fatalf("renderQueryResult: %v", err)
	}
	if !strings.Contains(out, "42") || !strings.Contains(out, "2025-08-17") {
		t.Errorf("scalar rendered as %q", out)
	}

	if _, err := renderQueryResult(&prom.QueryResult{ResultType: "banana"}, 100, nil); err == nil {
		t.Error("an unknown result type was rendered rather than refused")
	}
}

func TestRenderQueryResultSurfacesWarnings(t *testing.T) {
	out, err := renderQueryResult(vectorResult(t, ""), 100, []string{"partial results"})
	if err != nil {
		t.Fatalf("renderQueryResult: %v", err)
	}
	if !strings.Contains(out, "partial results") {
		t.Errorf("prometheus warning dropped:\n%s", out)
	}
}

func TestRenderMetricDescription(t *testing.T) {
	meta := []prom.Meta{{Type: "gauge", Help: "target health", Unit: "seconds"}}
	series := []map[string]string{
		{"__name__": "up", "job": "kubelet", "namespace": "fathom"},
		{"__name__": "up", "job": "api", "namespace": "default"},
	}

	out := renderMetricDescription("up", `{__name__="up"}`, meta, series, 500, 50, nil)

	for _, want := range []string{"gauge", "target health", "seconds", "Series: 2", "LABEL", "job", "namespace"} {
		if !strings.Contains(out, want) {
			t.Errorf("description is missing %q:\n%s", want, out)
		}
	}
	// __name__ is the metric's own name; listing it as a label to filter on
	// would send the model writing {__name__="up"} instead of up.
	if strings.Contains(out, "__name__") {
		t.Errorf("__name__ was listed as a label:\n%s", out)
	}
	if !strings.Contains(out, "api, kubelet") {
		t.Errorf("label values are not sorted:\n%s", out)
	}
}

// Hitting the cap is itself the answer to "is this metric high-cardinality",
// so it must be reported rather than rounded into an exact-looking number.
func TestRenderMetricDescriptionReportsCardinalityCap(t *testing.T) {
	var series []map[string]string
	for i := 0; i < 12; i++ {
		series = append(series, map[string]string{"__name__": "up", "pod": "pod-" + itoa(i)})
	}

	out := renderMetricDescription("up", `{__name__="up"}`, nil, series, 10, 3, nil)

	if !strings.Contains(out, "Series: 10 or more (capped)") {
		t.Errorf("cardinality cap not reported:\n%s", out)
	}
	if !strings.Contains(out, "and 7 more") {
		t.Errorf("label values not capped at 3 with a remainder:\n%s", out)
	}
	if !strings.Contains(out, "first 10 series only") {
		t.Errorf("partial label discovery not disclosed:\n%s", out)
	}
	if !strings.Contains(out, "no metadata registered") {
		t.Errorf("a metric with no metadata is not explained:\n%s", out)
	}
}

func TestRenderMetricDescriptionNoSeries(t *testing.T) {
	out := renderMetricDescription("up", `{__name__="up",job="gone"}`, nil, nil, 500, 50, nil)
	if !strings.Contains(out, "No series currently match") {
		t.Errorf("an empty series set rendered as a table:\n%s", out)
	}
}

func TestJoinBounded(t *testing.T) {
	set := map[string]struct{}{"c": {}, "a": {}, "b": {}}
	if got := joinBounded(set, 10); got != "a, b, c" {
		t.Errorf("joinBounded = %q, want sorted and complete", got)
	}
	if got := joinBounded(set, 2); got != "a, b (and 1 more)" {
		t.Errorf("joinBounded capped = %q", got)
	}
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}
