package tools

import (
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
