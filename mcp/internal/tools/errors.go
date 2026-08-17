package tools

import (
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

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
