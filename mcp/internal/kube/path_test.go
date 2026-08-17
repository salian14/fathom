package kube

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func mapping(group, version, resource string, namespaced bool) *meta.RESTMapping {
	scope := meta.RESTScopeRoot
	if namespaced {
		scope = meta.RESTScopeNamespace
	}
	return &meta.RESTMapping{
		Resource: schema.GroupVersionResource{Group: group, Version: version, Resource: resource},
		Scope:    scope,
	}
}

func TestPath(t *testing.T) {
	cases := []struct {
		name      string
		mapping   *meta.RESTMapping
		namespace string
		object    string
		want      string
	}{
		{
			name:    "core group list, all namespaces",
			mapping: mapping("", "v1", "pods", true),
			want:    "/api/v1/pods",
		},
		{
			name:      "core group, namespaced list",
			mapping:   mapping("", "v1", "pods", true),
			namespace: "kube-system",
			want:      "/api/v1/namespaces/kube-system/pods",
		},
		{
			name:      "core group, single object",
			mapping:   mapping("", "v1", "pods", true),
			namespace: "default",
			object:    "web-0",
			want:      "/api/v1/namespaces/default/pods/web-0",
		},
		{
			name:    "cluster-scoped resource takes no namespace segment",
			mapping: mapping("", "v1", "nodes", false),
			object:  "node-1",
			want:    "/api/v1/nodes/node-1",
		},
		{
			name:      "named group is served under /apis",
			mapping:   mapping("apps", "v1", "deployments", true),
			namespace: "fathom",
			object:    "agent",
			want:      "/apis/apps/v1/namespaces/fathom/deployments/agent",
		},
		{
			// A cluster-scoped resource must not gain a namespace segment even
			// if a namespace is somehow passed through.
			name:      "cluster-scoped ignores a stray namespace",
			mapping:   mapping("", "v1", "namespaces", false),
			namespace: "default",
			want:      "/api/v1/namespaces",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Path(tc.mapping, tc.namespace, tc.object); got != tc.want {
				t.Errorf("Path() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResourceKey(t *testing.T) {
	if got := ResourceKey(mapping("", "v1", "pods", true)); got != "pods" {
		t.Errorf("ResourceKey(core) = %q, want pods", got)
	}
	if got := ResourceKey(mapping("apps", "v1", "deployments", true)); got != "deployments.apps" {
		t.Errorf("ResourceKey(apps) = %q, want deployments.apps", got)
	}
}
