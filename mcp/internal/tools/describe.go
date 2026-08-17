package tools

import (
	"context"
	"fmt"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/client-go/rest"
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

	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	describer, ok := describe.DescriberFor(mapping.GroupVersionKind.GroupKind(), s.describeConfig(ctx))
	if !ok {
		describer, ok = describe.GenericDescriberFor(mapping, s.describeConfig(ctx))
		if !ok {
			return nil, nil, fmt.Errorf("cannot describe %s; use k8s_get to read the object instead",
				mapping.Resource.Resource)
		}
	}

	out, err := describer.Describe(namespace, in.Name, describe.DescriberSettings{
		ShowEvents: showEvents,
		ChunkSize:  500,
	})
	if err != nil {
		s.failed(ctx, "k8s_describe", mapping.Resource.Resource, err)
		return nil, nil, apiError(err, mapping.Resource.Resource, namespace, in.Name)
	}

	return text(truncate(out, s.cfg.MaxBytes, keepHead)), nil, nil
}

// describeConfig returns a REST config whose requests carry ctx.
//
// kubectl's describer builds its own clients and its Describe method takes no
// context, so without this its calls would run with context.Background(): the
// request timeout would not reach them, and they would show up as orphan root
// spans rather than children of the tool call. Wrapping here re-attaches the
// context at the transport, which is the only seam the describer leaves open.
//
// This replaces an earlier version that ran Describe on a detached goroutine
// and selected on ctx.Done(). That bounded the *caller* but never cancelled
// the work -- the goroutine kept querying the API server after the tool had
// already returned.
func (s *Server) describeConfig(ctx context.Context) *rest.Config {
	cfg := rest.CopyConfig(s.client.Config)
	cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			return rt.RoundTrip(r.WithContext(ctx))
		})
	})
	return cfg
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
