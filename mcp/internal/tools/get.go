package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/spellbound/fathom/mcp/internal/kube"
)

const tableAccept = "application/json;as=Table;v=v1;g=meta.k8s.io,application/json"

type getInput struct {
	Resource      string `json:"resource" jsonschema:"Resource type to read, e.g. \"pods\", \"nodes\", \"deployments.apps\". Singular, plural and short names all work."`
	Namespace     string `json:"namespace,omitempty" jsonschema:"Namespace to read from. Omit for the default namespace; ignored for cluster-scoped resources like nodes."`
	Name          string `json:"name,omitempty" jsonschema:"Name of a single object. Omit to list. With a name, the full object is returned as YAML; without one, a summary table is returned."`
	AllNamespaces bool   `json:"allNamespaces,omitempty" jsonschema:"List across every namespace. Only valid when listing (no name)."`
	LabelSelector string `json:"labelSelector,omitempty" jsonschema:"Label selector, e.g. \"app=nginx,tier!=cache\"."`
	FieldSelector string `json:"fieldSelector,omitempty" jsonschema:"Field selector, e.g. \"status.phase=Running\"."`
	Limit         int64  `json:"limit,omitempty" jsonschema:"Maximum number of objects to list. Bounded by the server's own limit."`
}

func (s *Server) handleGet(ctx context.Context, _ *mcp.CallToolRequest, in getInput) (*mcp.CallToolResult, any, error) {
	if err := ValidateName(in.Name); err != nil {
		return nil, nil, err
	}
	if err := ValidateNamespace(in.Namespace); err != nil {
		return nil, nil, err
	}
	if err := ValidateLabelSelector(in.LabelSelector); err != nil {
		return nil, nil, err
	}
	if err := ValidateFieldSelector(in.FieldSelector); err != nil {
		return nil, nil, err
	}

	allNamespaces := in.AllNamespaces
	if in.Name != "" {
		if allNamespaces {
			return nil, nil, fmt.Errorf("allNamespaces cannot be combined with name; " +
				"a named object is fetched from exactly one namespace")
		}
		if in.LabelSelector != "" || in.FieldSelector != "" {
			return nil, nil, fmt.Errorf("selectors cannot be combined with name; " +
				"drop the name to list with a selector")
		}
	}

	mapping, err := s.resolveAllowed(in.Resource)
	if err != nil {
		return nil, nil, err
	}

	namespace, err := s.namespaceFor(mapping, in.Namespace, allNamespaces)
	if err != nil {
		return nil, nil, err
	}

	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	if in.Name != "" {
		out, err := s.getObject(ctx, mapping, namespace, in.Name)
		if err != nil {
			return nil, nil, err
		}
		return text(out), nil, nil
	}

	out, err := s.listObjects(ctx, mapping, namespace, in, allNamespaces)
	if err != nil {
		return nil, nil, err
	}
	return text(out), nil, nil
}

func (s *Server) getObject(ctx context.Context, mapping *meta.RESTMapping, namespace, name string) (string, error) {
	raw, err := s.client.REST.Get().
		AbsPath(kube.Path(mapping, namespace, name)).
		SetHeader("Accept", "application/json").
		DoRaw(ctx)
	if err != nil {
		s.failed(ctx, "k8s_get", mapping.Resource.Resource, err)
		return "", apiError(err, mapping.Resource.Resource, namespace, name)
	}

	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", fmt.Errorf("decoding %s/%s: %w", mapping.Resource.Resource, name, err)
	}
	stripNoise(obj)

	out, err := yaml.Marshal(obj)
	if err != nil {
		return "", fmt.Errorf("encoding %s/%s as YAML: %w", mapping.Resource.Resource, name, err)
	}
	return truncate(string(out), s.cfg.MaxBytes, keepHead), nil
}

func (s *Server) listObjects(ctx context.Context, mapping *meta.RESTMapping, namespace string, in getInput, allNamespaces bool) (string, error) {
	limit := in.Limit
	if limit <= 0 || limit > s.cfg.ListLimit {
		limit = s.cfg.ListLimit
	}

	req := s.client.REST.Get().
		AbsPath(kube.Path(mapping, namespace, "")).
		SetHeader("Accept", tableAccept).
		Param("limit", strconv.FormatInt(limit, 10)).
		Param("includeObject", "Metadata")

	if in.LabelSelector != "" {
		req = req.Param("labelSelector", in.LabelSelector)
	}
	if in.FieldSelector != "" {
		req = req.Param("fieldSelector", in.FieldSelector)
	}

	raw, err := req.DoRaw(ctx)
	if err != nil {
		s.failed(ctx, "k8s_get", mapping.Resource.Resource, err)
		return "", apiError(err, mapping.Resource.Resource, namespace, "")
	}

	var table metav1.Table
	if err := json.Unmarshal(raw, &table); err != nil {
		return "", fmt.Errorf("decoding %s table: %w", mapping.Resource.Resource, err)
	}

	scope := "namespace " + namespace
	if namespace == "" {
		if kube.Namespaced(mapping) {
			scope = "all namespaces"
		} else {
			scope = "cluster scope"
		}
	}

	if len(table.Rows) == 0 {
		return fmt.Sprintf("No %s found in %s.\n", mapping.Resource.Resource, scope), nil
	}

	rendered := renderTable(&table, allNamespaces && kube.Namespaced(mapping))
	if table.Continue != "" || int64(len(table.Rows)) >= limit {
		rendered += fmt.Sprintf("\n[showing the first %d %s in %s; more exist]\n",
			len(table.Rows), mapping.Resource.Resource, scope)
	}
	return truncate(rendered, s.cfg.MaxBytes, keepHead), nil
}

func renderTable(table *metav1.Table, withNamespace bool) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 3, ' ', 0)

	keep := make([]int, 0, len(table.ColumnDefinitions))
	headers := make([]string, 0, len(table.ColumnDefinitions))
	if withNamespace {
		headers = append(headers, "NAMESPACE")
	}
	for i, col := range table.ColumnDefinitions {
		if col.Priority != 0 {
			continue
		}
		keep = append(keep, i)
		headers = append(headers, strings.ToUpper(col.Name))
	}
	fmt.Fprintln(w, strings.Join(headers, "\t"))

	for _, row := range table.Rows {
		cells := make([]string, 0, len(headers))
		if withNamespace {
			cells = append(cells, rowNamespace(row))
		}
		for _, i := range keep {
			if i >= len(row.Cells) {
				cells = append(cells, "<none>")
				continue
			}
			cells = append(cells, cellString(row.Cells[i]))
		}
		fmt.Fprintln(w, strings.Join(cells, "\t"))
	}

	_ = w.Flush()
	return b.String()
}

func rowNamespace(row metav1.TableRow) string {
	if len(row.Object.Raw) == 0 {
		return "<unknown>"
	}
	var pom struct {
		Metadata struct {
			Namespace string `json:"namespace"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(row.Object.Raw, &pom); err != nil || pom.Metadata.Namespace == "" {
		return "<unknown>"
	}
	return pom.Metadata.Namespace
}

func cellString(v any) string {
	switch t := v.(type) {
	case nil:
		return "<none>"
	case string:
		if t == "" {
			return "<none>"
		}
		return t
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprint(t)
	}
}

// noisyMetadata is the bookkeeping the API server keeps on every object for
// its own benefit: concurrency tokens, identity handles and field-ownership
// records. None of it says anything about what the object is or how it is
// doing, and managedFields alone is routinely larger than the spec.
//
// What is deliberately absent from this list, because it is state:
// creationTimestamp (age), generation (compare with status.observedGeneration
// to see whether a controller has caught up), deletionTimestamp and finalizers
// (why something is stuck Terminating), labels, and ownerReferences.
var noisyMetadata = []string{
	"managedFields",
	"resourceVersion",
	"uid",
	"selfLink",
	"generateName",
}

// noisyAnnotations repeat the object inside itself.
var noisyAnnotations = []string{
	"kubectl.kubernetes.io/last-applied-configuration",
}

// noisyOwnerRef trims each owner reference down to what names the owner. The
// reference itself stays: "which ReplicaSet owns this pod" is how a model walks
// from a symptom to its cause.
var noisyOwnerRef = []string{
	"uid",
	"blockOwnerDeletion",
}

// stripNoise removes what costs context and describes nothing, so that the
// byte cap is spent on spec and status.
//
// Beyond metadata it makes exactly two cuts, each a block the cluster wrote
// rather than the workload's author: the image cache listing on a Node and the
// injected service account token volume on a Pod. Everything else in spec and
// status passes through as the API server returned it, because guessing which
// fields are "defaults" would mean hiding real configuration from the model.
func stripNoise(obj map[string]any) {
	md, ok := obj["metadata"].(map[string]any)
	if !ok {
		return
	}
	for _, k := range noisyMetadata {
		delete(md, k)
	}

	if ann, ok := md["annotations"].(map[string]any); ok {
		for _, k := range noisyAnnotations {
			delete(ann, k)
		}
		if len(ann) == 0 {
			delete(md, "annotations")
		}
	}

	if owners, ok := md["ownerReferences"].([]any); ok {
		for _, o := range owners {
			ref, ok := o.(map[string]any)
			if !ok {
				continue
			}
			for _, k := range noisyOwnerRef {
				delete(ref, k)
			}
		}
	}

	// Core-group kinds only: a CRD that happens to be called Pod or Node owes
	// nothing to these shapes.
	if obj["apiVersion"] == "v1" {
		switch obj["kind"] {
		case "Node":
			stripNodeImages(obj)
		case "Pod":
			stripTokenVolume(obj)
		}
	}
}

// stripNodeImages drops status.images: every image the kubelet has cached,
// with each of its tags and digests and its size. It is the largest block on a
// Node, it sits ahead of nodeInfo in the output, and it says what has been
// pulled at some point -- not what is running, which the pods say.
func stripNodeImages(node map[string]any) {
	if status, ok := node["status"].(map[string]any); ok {
		delete(status, "images")
	}
}

// tokenVolumePrefix is the name the ServiceAccount admission controller gives
// the projected token volume it injects, followed by a random suffix.
const tokenVolumePrefix = "kube-api-access-"

// stripTokenVolume drops the injected service account token volume and every
// mount of it. The block is some twenty lines, identical on every pod that
// has it, and describes the admission controller rather than the workload.
//
// A volume is only dropped when it has both the injected name and a projected
// source, so a volume an author chose to call kube-api-access-something is
// kept. Mounts are dropped by the names actually removed, never by prefix.
func stripTokenVolume(pod map[string]any) {
	spec, ok := pod["spec"].(map[string]any)
	if !ok {
		return
	}

	removed := map[string]bool{}
	spec["volumes"] = filterMaps(spec["volumes"], func(v map[string]any) bool {
		name, _ := v["name"].(string)
		_, projected := v["projected"]
		if strings.HasPrefix(name, tokenVolumePrefix) && projected {
			removed[name] = true
			return false
		}
		return true
	})
	if spec["volumes"] == nil {
		delete(spec, "volumes")
	}
	if len(removed) == 0 {
		return
	}

	for _, field := range []string{"containers", "initContainers", "ephemeralContainers"} {
		containers, _ := spec[field].([]any)
		for _, c := range containers {
			container, ok := c.(map[string]any)
			if !ok {
				continue
			}
			container["volumeMounts"] = filterMaps(container["volumeMounts"], func(m map[string]any) bool {
				name, _ := m["name"].(string)
				return !removed[name]
			})
			if container["volumeMounts"] == nil {
				delete(container, "volumeMounts")
			}
		}
	}
}

// filterMaps returns the entries of a decoded JSON list for which keep is
// true, or nil when none are left or v was not a list. Entries that are not
// objects are kept: this strips what it recognises and nothing else.
func filterMaps(v any, keep func(map[string]any) bool) any {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]any, 0, len(list))
	for _, item := range list {
		if m, ok := item.(map[string]any); ok && !keep(m) {
			continue
		}
		out = append(out, item)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
