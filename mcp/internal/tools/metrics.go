package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/spellbound/fathom/mcp/internal/prom"
)

// Tool names, used as metric dimensions as well as registrations.
const (
	toolListMetrics    = "list_metrics"
	toolDescribeMetric = "describe_metric"
	toolRunPromQL      = "run_promql"
)

// registerMetrics adds the Prometheus-backed tools. Called from Register only
// when a Prometheus URL is configured.
func (s *Server) registerMetrics(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: toolListMetrics,
		Description: "List the metric names Prometheus is collecting. This is the cheap way " +
			"to find out what is measurable before querying anything. Filter with 'contains' " +
			"to search by substring, or 'match' to restrict to series matching a selector. " +
			"Read-only.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, Title: "List Prometheus metrics"},
	}, s.handleListMetrics)

	mcp.AddTool(srv, &mcp.Tool{
		Name: toolDescribeMetric,
		Description: "Describe one metric: its type, help text and unit, how many series it " +
			"has, and the label names and values available on it. Call this before writing a " +
			"query -- it is what tells you which labels exist to filter or aggregate by, so " +
			"you do not have to guess. Read-only.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, Title: "Describe a Prometheus metric"},
	}, s.handleDescribeMetric)

	mcp.AddTool(srv, &mcp.Tool{
		Name: toolRunPromQL,
		Description: "Run an instant PromQL query and return the result. The query must be " +
			"valid PromQL; if Prometheus rejects it, the error says where. Prefer an " +
			"aggregation such as 'sum by (namespace) (...)' over selecting raw series, " +
			"because the number of series returned is capped. Read-only: this cannot change " +
			"anything in Prometheus or the cluster.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, Title: "Run a PromQL query"},
	}, s.handleRunPromQL)
}

type listMetricsInput struct {
	Contains string `json:"contains,omitempty" jsonschema:"Only return metric names containing this substring, e.g. \"memory\". Case-insensitive."`
	Match    string `json:"match,omitempty" jsonschema:"Restrict to metrics whose series match this PromQL selector, e.g. \"{job=\\\"kubelet\\\"}\". Omit to consider every series."`
	Limit    int    `json:"limit,omitempty" jsonschema:"Maximum number of metric names to return. Bounded by the server's own limit."`
}

func (s *Server) handleListMetrics(ctx context.Context, _ *mcp.CallToolRequest, in listMetricsInput) (*mcp.CallToolResult, any, error) {
	match, err := ValidateSelector(in.Match)
	if err != nil {
		return nil, nil, err
	}
	if len(in.Contains) > maxArgLen {
		return nil, nil, fmt.Errorf("contains is too long (%d chars, max %d)", len(in.Contains), maxArgLen)
	}

	limit := in.Limit
	if limit <= 0 || limit > s.cfg.MetricsListLimit {
		limit = s.cfg.MetricsListLimit
	}

	// The label-values endpoint wants a whole selector; ValidateSelector hands
	// back the inner matcher list so each caller wraps it its own way.
	selector := ""
	if match != "" {
		selector = "{" + match + "}"
	}

	span := trace.SpanFromContext(ctx)
	if selector != "" {
		span.SetAttributes(attribute.String("prometheus.match", selector))
	}

	// A payload ceiling well above the display limit, so the "N of M" trailer
	// below reports a real total instead of the limit repeated back. Metric
	// *names* are a far smaller set than series, so this stays cheap.
	fetch := s.cfg.MetricsListLimit * 10

	ctx, cancel := s.promTimeout(ctx)
	defer cancel()

	names, warnings, err := s.prom.MetricNames(ctx, selector, fetch)
	if err != nil {
		s.promFailed(ctx, toolListMetrics, prom.EndpointLabelValues, err)
		return nil, nil, promError(err, "listing metric names")
	}
	capped := len(names) >= fetch

	if in.Contains != "" {
		needle := strings.ToLower(in.Contains)
		kept := names[:0]
		for _, n := range names {
			if strings.Contains(strings.ToLower(n), needle) {
				kept = append(kept, n)
			}
		}
		names = kept
	}
	sort.Strings(names)

	if len(names) == 0 {
		return text(noMetricsMessage(in.Contains, selector)), nil, nil
	}

	total := len(names)
	shown := names
	if total > limit {
		shown = names[:limit]
	}

	var b strings.Builder
	writeWarnings(&b, warnings)
	for _, n := range shown {
		b.WriteString(n)
		b.WriteByte('\n')
	}
	if len(shown) < total {
		fmt.Fprintf(&b, "\n[showing %d of %d matching metrics; narrow with 'contains' or 'match']\n",
			len(shown), total)
	} else if capped {
		fmt.Fprintf(&b, "\n[prometheus returned its own maximum of %d names; more may exist]\n", fetch)
	}

	return text(truncate(b.String(), s.cfg.MaxBytes, keepHead)), nil, nil
}

func noMetricsMessage(contains, match string) string {
	switch {
	case contains != "" && match != "":
		return fmt.Sprintf("No metric names contain %q among series matching %s.\n", contains, match)
	case contains != "":
		return fmt.Sprintf("No metric names contain %q. Try a shorter substring, or call "+
			"list_metrics with no filter to see what is collected.\n", contains)
	case match != "":
		return fmt.Sprintf("No metrics have series matching %s.\n", match)
	default:
		return "Prometheus is not reporting any metric names. It may have no targets yet.\n"
	}
}

type describeMetricInput struct {
	Metric string `json:"metric" jsonschema:"Name of the metric to describe, e.g. \"container_memory_working_set_bytes\". Required. Use list_metrics to find one."`
	Match  string `json:"match,omitempty" jsonschema:"Extra label matchers to narrow which series are inspected, e.g. \"namespace=\\\"fathom\\\"\". Braces are optional."`
}

func (s *Server) handleDescribeMetric(ctx context.Context, _ *mcp.CallToolRequest, in describeMetricInput) (*mcp.CallToolResult, any, error) {
	if in.Metric == "" {
		return nil, nil, fmt.Errorf("metric is required; use list_metrics to find a metric name")
	}
	if err := ValidateMetricName(in.Metric); err != nil {
		return nil, nil, err
	}
	match, err := ValidateSelector(in.Match)
	if err != nil {
		return nil, nil, err
	}

	selector := metricSelector(in.Metric, match)

	span := trace.SpanFromContext(ctx)
	span.SetAttributes(attribute.String("prometheus.metric.name", in.Metric))

	ctx, cancel := s.promTimeout(ctx)
	defer cancel()

	// Metadata is advisory: recording rules and many exporters carry none, and
	// a metric with no metadata is still worth describing. A failure here is
	// recorded but does not sink the call.
	meta, _, err := s.prom.Metadata(ctx, in.Metric)
	if err != nil {
		s.promFailed(ctx, toolDescribeMetric, prom.EndpointMetadata, err)
		meta = nil
	}

	// One series call answers three questions at once -- which labels exist,
	// what values they take, and how many series there are -- where asking the
	// label endpoints would cost one round trip per label.
	series, warnings, err := s.prom.Series(ctx, selector, s.cfg.MetricsSeriesLimit+1)
	if err != nil {
		s.promFailed(ctx, toolDescribeMetric, prom.EndpointSeries, err)
		return nil, nil, promError(err, fmt.Sprintf("describing metric %q", in.Metric))
	}

	out := renderMetricDescription(in.Metric, selector, meta[in.Metric], series,
		s.cfg.MetricsSeriesLimit, s.cfg.MetricsLabelValues, warnings)
	return text(truncate(out, s.cfg.MaxBytes, keepHead)), nil, nil
}

// renderMetricDescription builds the model-facing description. Split out from
// the handler so the formatting and the bounding are testable without a server.
func renderMetricDescription(metric, selector string, meta []prom.Meta, series []map[string]string,
	seriesLimit, valueLimit int, warnings []string) string {

	var b strings.Builder
	writeWarnings(&b, warnings)

	b.WriteString(metric + "\n")
	if len(meta) == 0 {
		b.WriteString("  (no metadata registered -- usual for recording rules and some exporters)\n")
	}
	for _, m := range meta {
		if m.Type != "" {
			fmt.Fprintf(&b, "  type: %s\n", m.Type)
		}
		if m.Unit != "" {
			fmt.Fprintf(&b, "  unit: %s\n", m.Unit)
		}
		if m.Help != "" {
			fmt.Fprintf(&b, "  help: %s\n", m.Help)
		}
	}

	if len(series) == 0 {
		fmt.Fprintf(&b, "\nNo series currently match %s. The metric is known to Prometheus but "+
			"nothing is reporting it right now, or the matchers exclude everything.\n", selector)
		return b.String()
	}

	// A cap hit is itself the answer to "is this metric high-cardinality",
	// so it is reported rather than hidden behind an exact-looking number.
	truncated := len(series) > seriesLimit
	if truncated {
		series = series[:seriesLimit]
		fmt.Fprintf(&b, "\nSeries: %d or more (capped)\n", seriesLimit)
	} else {
		fmt.Fprintf(&b, "\nSeries: %d\n", len(series))
	}

	values := map[string]map[string]struct{}{}
	for _, labels := range series {
		for name, value := range labels {
			if name == labelMetricName {
				continue
			}
			if values[name] == nil {
				values[name] = map[string]struct{}{}
			}
			values[name][value] = struct{}{}
		}
	}

	if len(values) == 0 {
		b.WriteString("\nThis metric has no labels beyond its name.\n")
		return b.String()
	}

	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)

	b.WriteString("\n")
	w := tabwriter.NewWriter(&b, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "LABEL\tVALUES")
	for _, name := range names {
		fmt.Fprintf(w, "%s\t%s\n", name, joinBounded(values[name], valueLimit))
	}
	_ = w.Flush()

	if truncated {
		b.WriteString("\n[label values are drawn from the first " +
			fmt.Sprint(seriesLimit) + " series only; others may exist]\n")
	}
	return b.String()
}

// joinBounded renders a value set in sorted order, capped, with a count of
// what was left out.
func joinBounded(set map[string]struct{}, limit int) string {
	vals := make([]string, 0, len(set))
	for v := range set {
		vals = append(vals, v)
	}
	sort.Strings(vals)

	if limit > 0 && len(vals) > limit {
		rest := len(vals) - limit
		return strings.Join(vals[:limit], ", ") + fmt.Sprintf(" (and %d more)", rest)
	}
	return strings.Join(vals, ", ")
}

type runPromQLInput struct {
	Query string `json:"query" jsonschema:"PromQL expression to evaluate, e.g. \"sum by (namespace) (kube_pod_info)\". Required. Evaluated at a single instant, so use a range only inside a function such as rate(x[5m])."`
	Time  string `json:"time,omitempty" jsonschema:"Instant to evaluate at, as RFC3339, e.g. \"2026-08-17T10:00:00Z\". Omit for now."`
}

func (s *Server) handleRunPromQL(ctx context.Context, _ *mcp.CallToolRequest, in runPromQLInput) (*mcp.CallToolResult, any, error) {
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return nil, nil, fmt.Errorf("query is required; describe_metric shows a metric's labels " +
			"if you need them to write one")
	}
	if len(query) > s.cfg.PromQLMaxQueryLength {
		return nil, nil, fmt.Errorf("query is too long (%d chars, max %d)",
			len(query), s.cfg.PromQLMaxQueryLength)
	}

	var at time.Time
	if in.Time != "" {
		parsed, err := time.Parse(time.RFC3339, in.Time)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid time %q: want RFC3339, e.g. "+
				"\"2026-08-17T10:00:00Z\"", in.Time)
		}
		at = parsed
	}

	// The query is unbounded text, so it is a span attribute and never a
	// metric dimension.
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(attribute.String("prometheus.query", query))

	ctx, cancel := s.promTimeout(ctx)
	defer cancel()

	// Prometheus gets the deadline too: without it the server keeps evaluating
	// a query that nobody is waiting for any more.
	result, warnings, err := s.prom.Query(ctx, query, at, s.cfg.PrometheusTimeout)
	if err != nil {
		s.promFailed(ctx, toolRunPromQL, prom.EndpointQuery, err)
		return nil, nil, promError(err, "running the query")
	}

	span.SetAttributes(attribute.String("prometheus.result.type", result.ResultType))

	out, err := renderQueryResult(result, s.cfg.PromQLMaxSeries, warnings)
	if err != nil {
		return nil, nil, err
	}
	return text(truncate(out, s.cfg.MaxBytes, keepHead)), nil, nil
}

// maxMatrixPoints bounds the samples printed per series for a range result.
// A matrix only appears when the expression is itself a range selector, which
// is not the shape this tool is for, so the budget is deliberately small.
const maxMatrixPoints = 10

func renderQueryResult(result *prom.QueryResult, maxSeries int, warnings []string) (string, error) {
	var b strings.Builder
	writeWarnings(&b, warnings)

	switch result.ResultType {
	case prom.ResultVector:
		samples, err := result.AsVector()
		if err != nil {
			return "", err
		}
		if len(samples) == 0 {
			b.WriteString("The query matched no series.\n")
			return b.String(), nil
		}

		total := len(samples)
		if total > maxSeries {
			samples = samples[:maxSeries]
		}

		w := tabwriter.NewWriter(&b, 0, 0, 3, ' ', 0)
		fmt.Fprintln(w, "SERIES\tVALUE")
		for _, sample := range samples {
			fmt.Fprintf(w, "%s\t%s\n", formatLabels(sample.Metric), sample.Value.Value)
		}
		_ = w.Flush()

		if len(samples) < total {
			fmt.Fprintf(&b, "\n[showing %d of %d series; aggregate with sum by (...) to see all "+
				"of it at once]\n", len(samples), total)
		}
		return b.String(), nil

	case prom.ResultMatrix:
		streams, err := result.AsMatrix()
		if err != nil {
			return "", err
		}
		if len(streams) == 0 {
			b.WriteString("The query matched no series.\n")
			return b.String(), nil
		}

		total := len(streams)
		if total > maxSeries {
			streams = streams[:maxSeries]
		}

		for _, stream := range streams {
			b.WriteString(formatLabels(stream.Metric) + "\n")
			points := stream.Values
			dropped := 0
			if len(points) > maxMatrixPoints {
				dropped = len(points) - maxMatrixPoints
				points = points[len(points)-maxMatrixPoints:]
			}
			for _, p := range points {
				fmt.Fprintf(&b, "  %s  %s\n", p.Time().Format(time.RFC3339), p.Value)
			}
			if dropped > 0 {
				fmt.Fprintf(&b, "  [%d earlier points not shown]\n", dropped)
			}
		}

		if len(streams) < total {
			fmt.Fprintf(&b, "\n[showing %d of %d series]\n", len(streams), total)
		}
		return b.String(), nil

	case prom.ResultScalar, prom.ResultString:
		pair, err := result.AsScalar()
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "%s (at %s)\n", pair.Value, pair.Time().Format(time.RFC3339))
		return b.String(), nil

	default:
		return "", fmt.Errorf("prometheus returned an unexpected result type %q", result.ResultType)
	}
}

const labelMetricName = "__name__"

// formatLabels renders a label set the way PromQL writes one, so the model can
// paste it back into a query: name{key="value",...}, keys sorted.
func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return "{}"
	}

	name := labels[labelMetricName]
	keys := make([]string, 0, len(labels))
	for k := range labels {
		if k != labelMetricName {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	if len(keys) == 0 {
		if name == "" {
			return "{}"
		}
		return name
	}

	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, fmt.Sprintf("%s=%q", k, labels[k]))
	}
	return name + "{" + strings.Join(pairs, ",") + "}"
}

// metricSelector builds the series selector for a metric plus optional extra
// matchers. Built here rather than accepting a whole selector from the model,
// so describe_metric always describes the metric it was asked about.
func metricSelector(metric, match string) string {
	if match == "" {
		return fmt.Sprintf("{%s=%q}", labelMetricName, metric)
	}
	return fmt.Sprintf("{%s=%q,%s}", labelMetricName, metric, match)
}

// writeWarnings surfaces Prometheus's own warnings, which is how it reports
// partial results. A model told nothing would read a truncated answer as
// complete.
func writeWarnings(b *strings.Builder, warnings []string) {
	for _, w := range warnings {
		fmt.Fprintf(b, "[prometheus warning: %s]\n", w)
	}
	if len(warnings) > 0 {
		b.WriteByte('\n')
	}
}

func (s *Server) promTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.cfg.PrometheusTimeout)
}
