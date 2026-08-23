package tools

import "github.com/spellbound/fathom/mcp/internal/telemetry"

// MetricDocs describes the instrument this package defines.
//
// The three attribute keys are the dimension docs/conventions.md governs: both
// the Kubernetes and the Prometheus failure paths classify onto the same seven
// error classes and feed this one counter, precisely so the stream can be
// compared across backends rather than split into two that cannot.
func MetricDocs() []telemetry.MetricDoc {
	return []telemetry.MetricDoc{{
		Name:        "mcp.tool.errors",
		Kind:        "counter",
		Unit:        "{error}",
		Description: "Kubernetes API errors surfaced by a tool, by class.",
		// tool is a six-entry set; resource is an allowlisted Kubernetes kind
		// or one of four Prometheus endpoint names; error_class is one of
		// seven. Object names, selectors, metric names and PromQL queries are
		// unbounded and stay on the span.
		Attributes: []string{"tool", "resource", "error_class"},
	}}
}
