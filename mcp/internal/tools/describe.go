package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/kubectl/pkg/describe"
)

type describeInput struct {
	Resource   string `json:"resource" jsonschema:"Resource type of the object, e.g. \"pods\", \"nodes\", \"deployments.apps\"."`
	Name       string `json:"name" jsonschema:"Name of the object to describe. Required."`
	Namespace  string `json:"namespace,omitempty" jsonschema:"Namespace of the object. Omit for the default namespace; ignored for cluster-scoped resources."`
	ShowEvents *bool  `json:"showEvents,omitempty" jsonschema:"Include the object's recent events. Defaults to true; events are usually the reason to describe something."`
}

func (s *Server) handleDescribe(ctx context.Context, _ *mcp.CallToolRequest, in describeInput) (*mcp.CallToolResult, any, error) {
	if in.Name == "" {
		return nil, nil, fmt.Errorf("name is required; k8s_describe describes one object at a time. " +
			"Use k8s_get to list objects first")
	}
	if err := ValidateName(in.Name); err != nil {
		return nil, nil, err
	}
	if err := ValidateNamespace(in.Namespace); err != nil {
		return nil, nil, err
	}

	mapping, err := s.resolveAllowed(in.Resource)
	if err != nil {
		return nil, nil, err
	}

	namespace, err := s.namespaceFor(mapping, in.Namespace, false)
	if err != nil {
		return nil, nil, err
	}

	showEvents := true
	if in.ShowEvents != nil {
		showEvents = *in.ShowEvents
	}

	describer, ok := describe.DescriberFor(mapping.GroupVersionKind.GroupKind(), s.client.Config)
	if !ok {
		describer, ok = describe.GenericDescriberFor(mapping, s.client.Config)
		if !ok {
			return nil, nil, fmt.Errorf("cannot describe %s; use k8s_get to read the object instead",
				mapping.Resource.Resource)
		}
	}

	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	out, err := s.describeWithContext(ctx, describer, namespace, in.Name, showEvents)
	if err != nil {
		return nil, nil, apiError(err, mapping.Resource.Resource, namespace, in.Name)
	}

	return text(truncate(out, s.cfg.MaxBytes, keepHead)), nil, nil
}

func (s *Server) describeWithContext(ctx context.Context, d describe.ResourceDescriber, namespace, name string, showEvents bool) (string, error) {
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)

	go func() {
		out, err := d.Describe(namespace, name, describe.DescriberSettings{
			ShowEvents: showEvents,
			ChunkSize:  500,
		})
		done <- result{out, err}
	}()

	select {
	case r := <-done:
		return r.out, r.err
	case <-ctx.Done():
		return "", fmt.Errorf("describe timed out after %s: %w", s.cfg.RequestTimeout, ctx.Err())
	}
}
