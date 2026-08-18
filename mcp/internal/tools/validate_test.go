package tools

import (
	"strings"
	"testing"
)

// The resource argument becomes part of a discovery lookup and, once resolved,
// part of a URL path. These cases are the ones that would matter if the model
// were adversarial rather than merely confused.
func TestValidateResourceArg(t *testing.T) {
	valid := []string{
		"pods",
		"po",
		"Pod",
		"deployments.apps",
		"deployments.v1.apps",
		"ingresses.networking.k8s.io",
	}
	for _, s := range valid {
		if err := ValidateResourceArg(s); err != nil {
			t.Errorf("ValidateResourceArg(%q) = %v, want nil", s, err)
		}
	}

	invalid := []string{
		"",
		"pods/exec",        // subresource: must not be reachable through 'resource'
		"pods/log",         //
		"../../secrets",    // path traversal
		"pods?watch=true",  // query injection
		"pods namespaces",  // space
		"pods\nnamespaces", // newline
		"1pods",            // must start with a letter
		"pods%2fexec",      // percent-encoded slash
		"*",                // wildcard
	}
	for _, s := range invalid {
		if err := ValidateResourceArg(s); err == nil {
			t.Errorf("ValidateResourceArg(%q) = nil, want error", s)
		}
	}
}

func TestValidateNamespace(t *testing.T) {
	for _, s := range []string{"", "default", "kube-system", "a", "ns-1"} {
		if err := ValidateNamespace(s); err != nil {
			t.Errorf("ValidateNamespace(%q) = %v, want nil", s, err)
		}
	}
	for _, s := range []string{"Default", "kube_system", "ns/../other", "-ns", "ns.", "a b"} {
		if err := ValidateNamespace(s); err == nil {
			t.Errorf("ValidateNamespace(%q) = nil, want error", s)
		}
	}
}

func TestValidateName(t *testing.T) {
	for _, s := range []string{"", "web", "web-0", "my.app.example.com", "a1"} {
		if err := ValidateName(s); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", s, err)
		}
	}
	for _, s := range []string{"Web", "web/../etc", "web pod", "web:latest", "-web"} {
		if err := ValidateName(s); err == nil {
			t.Errorf("ValidateName(%q) = nil, want error", s)
		}
	}
}

func TestValidateSelectors(t *testing.T) {
	if err := ValidateLabelSelector("app=nginx,tier!=cache"); err != nil {
		t.Errorf("valid labelSelector rejected: %v", err)
	}
	if err := ValidateLabelSelector("app in ("); err == nil {
		t.Error("malformed labelSelector accepted")
	}
	if err := ValidateFieldSelector("status.phase=Running"); err != nil {
		t.Errorf("valid fieldSelector rejected: %v", err)
	}
	if err := ValidateFieldSelector("status.phase"); err == nil {
		t.Error("malformed fieldSelector accepted")
	}
}

func TestValidateMetricName(t *testing.T) {
	// Colons are legal in Prometheus names (they mark recording rules) and
	// dashes are not -- the opposite of the Kubernetes rules above, which is
	// exactly why this needs its own validator.
	for _, s := range []string{"up", "node_cpu_seconds_total", "job:rate5m", "_internal", "X1"} {
		if err := ValidateMetricName(s); err != nil {
			t.Errorf("ValidateMetricName(%q) = %v, want nil", s, err)
		}
	}

	invalid := []string{
		"",
		"1up",                 // must not start with a digit
		"node-cpu",            // dash
		"up{job=\"x\"}",       // a selector, not a name
		"up or vector(1)",     // an expression, not a name
		"../../etc",           // path traversal
		"up\nnode_cpu",        // newline
		"up&query=drop",       // query injection
		"metric with a space", //
		"überflüssig",         // non-ASCII
	}
	for _, s := range invalid {
		if err := ValidateMetricName(s); err == nil {
			t.Errorf("ValidateMetricName(%q) = nil, want error", s)
		}
	}
}

func TestValidateSelector(t *testing.T) {
	// Braces are optional on the way in and always absent on the way out, so
	// callers can splice the result into a selector of their own.
	tests := []struct{ in, want string }{
		{"", ""},
		{`job="kubelet"`, `job="kubelet"`},
		{`{job="kubelet"}`, `job="kubelet"`},
		{`  {job="kubelet",ns!="x"}  `, `job="kubelet",ns!="x"`},
		{`{}`, ""},
	}
	for _, tt := range tests {
		got, err := ValidateSelector(tt.in)
		if err != nil {
			t.Errorf("ValidateSelector(%q) = %v, want nil", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ValidateSelector(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}

	invalid := []string{
		"job=\"kubelet\"\nmalicious",          // newline
		"job=\"a\"} or {x=\"y\"",              // escapes its own braces
		"{{nested}}",                          //
		strings.Repeat("a", maxSelectorLen+1), // unbounded length
	}
	for _, s := range invalid {
		if _, err := ValidateSelector(s); err == nil {
			t.Errorf("ValidateSelector(%q) = nil, want error", s)
		}
	}
}
