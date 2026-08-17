package kube

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/spellbound/fathom/mcp/internal/telemetry"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	memory "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

type Client struct {
	Config    *rest.Config
	Clientset kubernetes.Interface
	REST      rest.Interface
	mapper    *restmapper.DeferredDiscoveryRESTMapper
	disco     discovery.CachedDiscoveryInterface
}

type Options struct {
	KubeconfigPath string
	UserAgent      string
	QPS            float32
	Burst          int

	// Trace wraps the REST transport so every call into the Kubernetes API
	// becomes a client span.
	Trace bool
}

func New(opts Options) (*Client, error) {
	cfg, err := restConfig(opts.KubeconfigPath)
	if err != nil {
		return nil, err
	}

	if opts.UserAgent != "" {
		cfg.UserAgent = opts.UserAgent
	}
	if opts.QPS > 0 {
		cfg.QPS = opts.QPS
	}
	if opts.Burst > 0 {
		cfg.Burst = opts.Burst
	}

	if opts.Trace {
		// One hook covers everything that talks to the API server: the
		// clientset, the raw REST client, discovery, pod log streams, and --
		// because Config is handed to it verbatim -- kubectl's describer.
		// It has to go on before NewForConfig, which snapshots the transport.
		cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
			return otelhttp.NewTransport(rt,
				otelhttp.WithFilter(telemetry.TraceKubeRequest),
				// Paths carry object names, so the default URL-based span name
				// would make every pod its own operation in the backend's UI.
				otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
					return "k8s " + r.Method
				}))
		})
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building clientset: %w", err)
	}

	cached := memory.NewMemCacheClient(clientset.Discovery())

	return &Client{
		Config:    cfg,
		Clientset: clientset,
		REST:      clientset.CoreV1().RESTClient(),
		mapper:    restmapper.NewDeferredDiscoveryRESTMapper(cached),
		disco:     cached,
	}, nil
}

func restConfig(kubeconfigPath string) (*rest.Config, error) {
	cfg, inClusterErr := rest.InClusterConfig()
	if inClusterErr == nil {
		return cfg, nil
	}

	if kubeconfigPath == "" {
		return nil, fmt.Errorf(
			"no in-cluster credentials (%w) and no kubeconfig configured; "+
				"set FATHOM_MCP_KUBECONFIG_PATH when running outside the cluster", inClusterErr)
	}

	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig %q (in-cluster config unavailable: %v): %w",
			kubeconfigPath, inClusterErr, err)
	}
	return cfg, nil
}

func (c *Client) Ping() error {
	_, err := c.disco.ServerVersion()
	return err
}

func (c *Client) Resolve(resourceArg string) (*meta.RESTMapping, error) {
	arg := strings.ToLower(strings.TrimSpace(resourceArg))
	if arg == "" {
		return nil, fmt.Errorf("resource is required")
	}

	mapping, err := c.resolve(arg)
	if err == nil {
		return mapping, nil
	}
	if !meta.IsNoMatchError(err) {
		return nil, fmt.Errorf("could not reach the Kubernetes API to look up resource type %q: %w",
			resourceArg, err)
	}

	c.mapper.Reset()
	c.disco.Invalidate()

	mapping, err = c.resolve(arg)
	if err != nil {
		if meta.IsNoMatchError(err) {
			return nil, fmt.Errorf("unknown resource type %q; the cluster does not serve it", resourceArg)
		}
		return nil, fmt.Errorf("could not reach the Kubernetes API to look up resource type %q: %w",
			resourceArg, err)
	}
	return mapping, nil
}

func (c *Client) resolve(arg string) (*meta.RESTMapping, error) {
	fullySpecified, groupResource := schema.ParseResourceArg(arg)

	var gvr schema.GroupVersionResource
	if fullySpecified != nil {
		if g, err := c.mapper.ResourceFor(*fullySpecified); err == nil {
			gvr = g
		}
	}
	if gvr.Empty() {
		g, err := c.mapper.ResourceFor(groupResource.WithVersion(""))
		if err != nil {
			return nil, err
		}
		gvr = g
	}

	gvk, err := c.mapper.KindFor(gvr)
	if err != nil {
		return nil, err
	}
	return c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
}

func ResourceKey(m *meta.RESTMapping) string {
	gvr := m.Resource
	if gvr.Group == "" {
		return gvr.Resource
	}
	return gvr.Resource + "." + gvr.Group
}

func Namespaced(m *meta.RESTMapping) bool {
	return m.Scope.Name() == meta.RESTScopeNameNamespace
}

func Path(m *meta.RESTMapping, namespace, name string) string {
	var b strings.Builder

	gvr := m.Resource
	if gvr.Group == "" {
		b.WriteString("/api/" + gvr.Version)
	} else {
		b.WriteString("/apis/" + gvr.Group + "/" + gvr.Version)
	}
	if Namespaced(m) && namespace != "" {
		b.WriteString("/namespaces/" + namespace)
	}
	b.WriteString("/" + gvr.Resource)
	if name != "" {
		b.WriteString("/" + name)
	}
	return b.String()
}
