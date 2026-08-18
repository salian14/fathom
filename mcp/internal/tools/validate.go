package tools

import (
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
)

var resourceArgRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9.-]*$`)

const maxArgLen = 253

func ValidateResourceArg(s string) error {
	switch {
	case s == "":
		return fmt.Errorf("resource is required (for example: pods, deployments.apps, nodes)")
	case len(s) > maxArgLen:
		return fmt.Errorf("resource is too long (%d chars, max %d)", len(s), maxArgLen)
	case !resourceArgRe.MatchString(s):
		return fmt.Errorf("invalid resource %q: expected a resource type such as "+
			"\"pods\" or \"deployments.apps\"", s)
	}
	return nil
}

func ValidateNamespace(s string) error {
	if s == "" {
		return nil
	}
	if errs := validation.IsDNS1123Label(s); len(errs) > 0 {
		return fmt.Errorf("invalid namespace %q: %s", s, strings.Join(errs, "; "))
	}
	return nil
}

func ValidateName(s string) error {
	if s == "" {
		return nil
	}
	if errs := validation.IsDNS1123Subdomain(s); len(errs) > 0 {
		return fmt.Errorf("invalid name %q: %s", s, strings.Join(errs, "; "))
	}
	return nil
}

func ValidateContainer(s string) error {
	if s == "" {
		return nil
	}
	if errs := validation.IsDNS1123Label(s); len(errs) > 0 {
		return fmt.Errorf("invalid container %q: %s", s, strings.Join(errs, "; "))
	}
	return nil
}

func ValidateLabelSelector(s string) error {
	if s == "" {
		return nil
	}
	if _, err := labels.Parse(s); err != nil {
		return fmt.Errorf("invalid labelSelector %q: %w", s, err)
	}
	return nil
}

func ValidateFieldSelector(s string) error {
	if s == "" {
		return nil
	}
	if _, err := fields.ParseSelector(s); err != nil {
		return fmt.Errorf("invalid fieldSelector %q: %w", s, err)
	}
	return nil
}

// Prometheus metric names are a different alphabet from Kubernetes names:
// colons are legal (they are conventionally recording rules) and dashes are
// not.
var metricNameRe = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)

// maxSelectorLen bounds a matcher list. Generous next to maxArgLen, because a
// selector legitimately carries several matchers.
const maxSelectorLen = 1024

func ValidateMetricName(s string) error {
	switch {
	case s == "":
		return fmt.Errorf("metric is required (for example: up, container_memory_working_set_bytes)")
	case len(s) > maxArgLen:
		return fmt.Errorf("metric is too long (%d chars, max %d)", len(s), maxArgLen)
	case !metricNameRe.MatchString(s):
		return fmt.Errorf("invalid metric name %q: expected a Prometheus metric name such as "+
			"\"up\" or \"node_cpu_seconds_total\"", s)
	}
	return nil
}

// ValidateSelector checks a PromQL matcher list and returns it normalised
// without its optional enclosing braces, so callers can splice it into a
// selector of their own.
//
// It deliberately does not parse the matchers. Prometheus is the authority on
// PromQL syntax and its errors name the offending character, which is far more
// use to the caller than anything reimplemented here could be; what this
// guards is the things Prometheus would not catch as syntax -- unbounded
// length, and control characters that have no business in a URL parameter.
func ValidateSelector(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if len(s) > maxSelectorLen {
		return "", fmt.Errorf("selector is too long (%d chars, max %d)", len(s), maxSelectorLen)
	}

	if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	for _, r := range s {
		if r == '\n' || r == '\r' || r == 0 || (r < 0x20 && r != '\t') {
			return "", fmt.Errorf("selector contains a control character; expected matchers such as "+
				"%s", `job="kubelet",namespace="fathom"`)
		}
	}
	if strings.ContainsAny(s, "{}") {
		return "", fmt.Errorf("selector has unbalanced or nested braces; pass matchers such as "+
			"%s, with at most one enclosing pair", `job="kubelet"`)
	}
	return s, nil
}
