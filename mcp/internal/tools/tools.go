import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/apimachinery/pkg/api/meta"

	"github.com/spellbound/fathom/mcp/internal/config"
	"github.com/spellbound/fathom/mcp/internal/kube"
)

type Server struct {
	cfg    *config.Config
	client *kube.Client
	log    *slog.Logger
}

func New(cfg *config.Config, client *kube.Client, log *slog.Logger) *Server {
	return &Server{cfg: cfg, client: client, log: log}
}

func (s *Server) Register(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "k8s_get",
		Description: "List or fetch Kubernetes objects. Without 'name' it returns a " +
			"kubectl-style table of matching objects, which is the cheap way to survey a " +
			"namespace. With 'name' it returns that single object as YAML. Read-only.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, Title: "Get Kubernetes objects"},
	}, s.handleGet)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "k8s_describe",
		Description: "Describe one Kubernetes object in the same human-readable form as " +
			"'kubectl describe', including its recent events. Use this to diagnose why " +
			"something is unhealthy: events explain pending, crash-looping and failed-mount " +
			"states that the object's YAML alone does not. Read-only.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, Title: "Describe a Kubernetes object"},
	}, s.handleDescribe)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "k8s_log",
		Description: "Read the tail of a container's log from a pod. Output is bounded, so " +
			"the most recent lines are returned and older ones are dropped. Set 'previous' " +
			"to read the log of the last terminated container after a restart. Read-only.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, Title: "Read pod logs"},
	}, s.handleLog)
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func (s *Server) resolveAllowed(resourceArg string) (*meta.RESTMapping, error) {
	if err := ValidateResourceArg(resourceArg); err != nil {
		return nil, err
	}

	mapping, err := s.client.Resolve(resourceArg)
	if err != nil {
		return nil, err
	}

	key := kube.ResourceKey(mapping)
	if !s.cfg.AllowsResource(key) {
		return nil, fmt.Errorf("resource %q (%s) is not available to this agent; "+
			"readable types are: %s", resourceArg, key, strings.Join(s.cfg.AllowedResources, ", "))
	}
	return mapping, nil
}

func (s *Server) namespaceFor(mapping *meta.RESTMapping, ns string, allNamespaces bool) (string, error) {
	if err := ValidateNamespace(ns); err != nil {
		return "", err
	}

	if !kube.Namespaced(mapping) {
		if ns != "" {
			return "", fmt.Errorf("%s is cluster-scoped; do not pass a namespace",
				mapping.Resource.Resource)
		}
		return "", nil
	}

	if allNamespaces {
		if len(s.cfg.AllowedNamespaces) > 0 {
			return "", fmt.Errorf("this agent is restricted to namespaces %s; "+
				"query one of them instead of allNamespaces",
				strings.Join(s.cfg.AllowedNamespaces, ", "))
		}
		return "", nil // empty namespace in the path == across all namespaces
	}

	if ns == "" {
		ns = s.cfg.DefaultNamespace
	}
	if !s.cfg.AllowsNamespace(ns) {
		return "", fmt.Errorf("namespace %q is not available to this agent; "+
			"readable namespaces are: %s", ns, strings.Join(s.cfg.AllowedNamespaces, ", "))
	}
	return ns, nil
}

func (s *Server) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.cfg.RequestTimeout)
}

func truncate(s string, max int, keep truncateEnd) string {
	if max <= 0 || len(s) <= max {
		return s
	}

	const note = "\n[truncated: output exceeded the size limit]\n"
	budget := max - len(note)
	if budget < 0 {
		budget = 0
	}

	if keep == keepTail {
		cut := s[len(s)-budget:]
		if i := strings.IndexByte(cut, '\n'); i >= 0 && i < len(cut)-1 {
			cut = cut[i+1:]
		}
		return note + cut
	}

	cut := s[:budget]
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i]
	}
	return cut + note
}

type truncateEnd int

const (
	keepHead truncateEnd = iota
	keepTail
)
