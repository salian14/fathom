package tools

import (
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
