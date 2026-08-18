package tools

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/spellbound/fathom/mcp/internal/prom"
)

// ErrorClass is the last point at which a Kubernetes error is still typed --
// apiError rewrites it into prose immediately afterwards. Pin the mapping, so
// a metric dimension cannot silently change meaning.
func TestErrorClass(t *testing.T) {
	gr := schema.GroupResource{Resource: "pods"}

	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, errNone},
		{"not found", apierrors.NewNotFound(gr, "web"), errNotFound},
		{"forbidden", apierrors.NewForbidden(gr, "web", errors.New("nope")), errForbidden},
		{"unauthorized", apierrors.NewUnauthorized("no token"), errUnauthorized},
		{"timeout", apierrors.NewTimeoutError("slow", 1), errTimeout},
		{"server timeout", apierrors.NewServerTimeout(gr, "get", 1), errTimeout},
		{"too many requests", apierrors.NewTooManyRequests("busy", 1), errTimeout},
		{"invalid", apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, "web", nil), errInvalid},
		{"bad request", apierrors.NewBadRequest("malformed"), errInvalid},
		{"conflict", apierrors.NewConflict(gr, "web", errors.New("boom")), errOther},
		{"plain error", errors.New("connection refused"), errOther},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ErrorClass(tt.err); got != tt.want {
				t.Errorf("ErrorClass(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

// The classes are metric dimensions, so the set must stay small and stable.
func TestErrorClassesAreBounded(t *testing.T) {
	classes := map[string]bool{
		errNone: true, errNotFound: true, errForbidden: true, errUnauthorized: true,
		errTimeout: true, errInvalid: true, errOther: true,
	}
	if len(classes) != 7 {
		t.Fatalf("expected 7 distinct error classes, got %d", len(classes))
	}

	// A status error the switch does not name must land in "other" rather than
	// inventing a new label.
	err := &apierrors.StatusError{ErrStatus: metav1.Status{Code: 418, Reason: "Teapot"}}
	if got := ErrorClass(err); got != errOther {
		t.Errorf("unrecognised status = %q, want %q", got, errOther)
	}
}

// PromErrorClass shares its label set with ErrorClass on purpose: both feed
// the same mcp.tool.errors stream, and a second vocabulary would split it into
// two things that cannot be compared.
func TestPromErrorClass(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, errNone},
		{"client deadline", &prom.APIError{Timeout: true}, errTimeout},
		{"bad data", &prom.APIError{Status: 400, ErrorType: "bad_data"}, errInvalid},
		{"execution", &prom.APIError{Status: 422, ErrorType: "execution"}, errInvalid},
		{"server timeout", &prom.APIError{Status: 503, ErrorType: "timeout"}, errTimeout},
		{"canceled", &prom.APIError{ErrorType: "canceled"}, errTimeout},
		{"unavailable", &prom.APIError{Status: 503, ErrorType: "unavailable"}, errOther},
		{"unauthorized", &prom.APIError{Status: 401}, errUnauthorized},
		{"forbidden", &prom.APIError{Status: 403}, errForbidden},
		{"not found", &prom.APIError{Status: 404}, errNotFound},
		{"rate limited", &prom.APIError{Status: 429}, errTimeout},
		{"unrecognised status", &prom.APIError{Status: 418}, errOther},
		{"wrapped", fmt.Errorf("wrapped: %w", &prom.APIError{Status: 404}), errNotFound},
		{"not an API error", errors.New("connection refused"), errOther},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PromErrorClass(tt.err)
			if got != tt.want {
				t.Errorf("PromErrorClass(%v) = %q, want %q", tt.err, got, tt.want)
			}
			// Whatever the input, the label must stay inside the closed set.
			switch got {
			case errNone, errNotFound, errForbidden, errUnauthorized, errTimeout, errInvalid, errOther:
			default:
				t.Errorf("PromErrorClass invented a new metric label %q", got)
			}
		})
	}
}

// A malformed query is the one error the model can act on, so Prometheus's own
// positioned message has to reach it intact.
func TestPromErrorKeepsPrometheusWording(t *testing.T) {
	err := promError(&prom.APIError{
		Status:    400,
		Endpoint:  prom.EndpointQuery,
		ErrorType: "bad_data",
		Message:   `parse error at char 8: unexpected ")"`,
	}, "running the query")

	if !strings.Contains(err.Error(), `parse error at char 8`) {
		t.Errorf("prometheus's own wording was swallowed: %v", err)
	}

	// A timeout instead needs advice, because there is nothing to correct.
	timeout := promError(&prom.APIError{Timeout: true, Endpoint: prom.EndpointQuery}, "running the query")
	if !strings.Contains(timeout.Error(), "Narrow the query") {
		t.Errorf("timeout error gives the model nothing to do: %v", timeout)
	}

	// A misconfigured endpoint must not read as a query problem.
	notFound := promError(&prom.APIError{Status: 404, Endpoint: prom.EndpointQuery}, "running the query")
	if !strings.Contains(notFound.Error(), "PROMETHEUS_URL") {
		t.Errorf("404 does not point at the configuration: %v", notFound)
	}
}
