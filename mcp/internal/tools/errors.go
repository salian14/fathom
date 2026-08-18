package tools

import (
	"errors"
	"fmt"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/spellbound/fathom/mcp/internal/prom"
)

// Error classes. Low cardinality by construction, so they are safe as both
// span attributes and metric dimensions.
const (
	errNone         = ""
	errNotFound     = "not_found"
	errForbidden    = "forbidden"
	errUnauthorized = "unauthorized"
	errTimeout      = "timeout"
	errInvalid      = "invalid"
	errOther        = "other"
)

// ErrorClass normalises a Kubernetes API error to one of a fixed set of
// labels.
//
// It exists because apiError immediately rewrites these errors into prose for
// the model, and prose cannot be grouped or counted. This is the last point at
// which the typed error is still available, so classify here and let apiError
// do its own job.
func ErrorClass(err error) string {
	switch {
	case err == nil:
		return errNone
	case apierrors.IsNotFound(err):
		return errNotFound
	case apierrors.IsForbidden(err):
		return errForbidden
	case apierrors.IsUnauthorized(err):
		return errUnauthorized
	case apierrors.IsTimeout(err), apierrors.IsServerTimeout(err), apierrors.IsTooManyRequests(err):
		return errTimeout
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return errInvalid
	default:
		return errOther
	}
}

// PromErrorClass normalises a Prometheus API failure onto the same fixed set
// of labels as ErrorClass.
//
// Deliberately the same seven constants rather than a parallel vocabulary: the
// mcp.tool.errors stream is shared by every tool, and a second set of class
// names would split it into two things that cannot be compared or alerted on
// together.
func PromErrorClass(err error) string {
	if err == nil {
		return errNone
	}

	var apiErr *prom.APIError
	if !errors.As(err, &apiErr) {
		return errOther
	}
	if apiErr.Timeout {
		return errTimeout
	}

	// errorType is the more precise signal and Prometheus sets it whenever it
	// answered at all, so it is consulted before the status code.
	switch apiErr.ErrorType {
	case "bad_data", "execution":
		return errInvalid
	case "timeout", "canceled":
		return errTimeout
	case "unavailable", "internal":
		return errOther
	}

	switch apiErr.Status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return errInvalid
	case http.StatusUnauthorized:
		return errUnauthorized
	case http.StatusForbidden:
		return errForbidden
	case http.StatusNotFound:
		return errNotFound
	case http.StatusTooManyRequests, http.StatusGatewayTimeout:
		return errTimeout
	default:
		return errOther
	}
}

// promError rewrites a Prometheus failure into something the model can act on.
//
// The one rule worth stating: when Prometheus rejected the query itself, its
// own message is passed through verbatim. Those messages are position
// annotated ("parse error at char 14: unexpected \"}\"") and are the single
// most useful thing the tool can say -- a model given that will fix its query
// on the next turn, where a generic "the query was invalid" leaves it guessing.
func promError(err error, what string) error {
	var apiErr *prom.APIError
	if !errors.As(err, &apiErr) {
		return fmt.Errorf("%s: %w", what, err)
	}

	switch {
	case apiErr.Timeout, apiErr.ErrorType == "timeout":
		return fmt.Errorf("%s: prometheus did not answer in time. Narrow the query -- "+
			"fewer series, a shorter range, or an aggregation such as sum by (...)", what)

	case apiErr.ErrorType == "bad_data":
		return fmt.Errorf("%s: prometheus rejected this: %s. Fix the expression and try again; "+
			"describe_metric shows a metric's label names if that is what is wrong",
			what, apiErr.Message)

	case apiErr.ErrorType == "execution":
		return fmt.Errorf("%s: prometheus could not evaluate the query: %s. This is usually too "+
			"many series at once -- aggregate with sum by (...) or add a label matcher",
			what, apiErr.Message)

	case apiErr.Status == http.StatusUnauthorized, apiErr.Status == http.StatusForbidden:
		return fmt.Errorf("%s: prometheus refused the request (HTTP %d). "+
			"This is a deployment matter, not something a different query fixes",
			what, apiErr.Status)

	case apiErr.Status == http.StatusNotFound:
		return fmt.Errorf("%s: the prometheus API path was not found (HTTP 404); "+
			"FATHOM_MCP_PROMETHEUS_URL may point at something that is not prometheus", what)

	case apiErr.Message != "":
		return fmt.Errorf("%s: %s", what, apiErr.Message)

	default:
		return fmt.Errorf("%s: %w", what, err)
	}
}

func apiError(err error, resource, namespace, name string) error {
	target := resource
	if name != "" {
		target = fmt.Sprintf("%s %q", resource, name)
	}
	if namespace != "" {
		target += fmt.Sprintf(" in namespace %q", namespace)
	}

	switch {
	case apierrors.IsNotFound(err):
		return fmt.Errorf("%s not found; list the %s first to check the name and namespace",
			target, resource)

	case apierrors.IsForbidden(err):
		return fmt.Errorf("not permitted to read %s: the agent's ClusterRole does not "+
			"grant this. This needs an RBAC change in the chart, not a different query", target)

	case apierrors.IsUnauthorized(err):
		return fmt.Errorf("the API server rejected the agent's credentials while reading %s; "+
			"the service account token may be missing or expired", target)

	case apierrors.IsTimeout(err), apierrors.IsServerTimeout(err), apierrors.IsTooManyRequests(err):
		return fmt.Errorf("the API server was too busy to answer for %s; retry once", target)

	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return fmt.Errorf("the API server rejected the request for %s: %v", target, err)

	default:
		return fmt.Errorf("reading %s: %w", target, err)
	}
}
