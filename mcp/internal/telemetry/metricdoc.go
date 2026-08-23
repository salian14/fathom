package telemetry

// MetricDoc describes one instrument a component defines.
//
// The published telemetry contract is the one place in contracts/ that is not
// taken from a live run, and the reason is worth stating: an instrument's
// attribute *keys* are chosen at record time, not at creation time, so no
// provider installed around startup can enumerate them. Exercising every path
// that records would mean running the service against a cluster.
//
// So the keys are declared, beside the instrument they belong to, and a test
// asserts the declaration against what the code actually emits. The declaration
// is checkable rather than merely believed, which is the property that matters:
// docs/conventions.md forbids high-cardinality labels, and a label added to a
// metric stream is exactly the change that should be visible in a diff.
type MetricDoc struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	Unit        string   `json:"unit"`
	Description string   `json:"description"`
	Attributes  []string `json:"attributes"`
}

// MetricDocs is what this package's instruments look like on the wire.
func MetricDocs() []MetricDoc {
	return []MetricDoc{{
		Name:        "mcp.server.operation.duration",
		Kind:        "histogram",
		Unit:        "s",
		Description: "Duration of an inbound MCP method call.",
		// All three are closed sets: method names come from the protocol,
		// tool names from a six-entry registry, and outcome from a fixed
		// vocabulary. The session id is deliberately absent and lives only on
		// the span, because it is unbounded.
		Attributes: []string{string(attrMethod), string(attrTool), "outcome"},
	}}
}
