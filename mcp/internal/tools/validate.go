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
