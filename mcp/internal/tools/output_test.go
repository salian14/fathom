package tools

import (
	"encoding/json"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestTruncateKeepsTheRightEnd(t *testing.T) {
	// Logs: the newest lines are at the end and must survive.
	log := "oldest\n" + strings.Repeat("filler line\n", 200) + "newest\n"
	got := truncate(log, 200, keepTail)
	if len(got) > 200 {
		t.Fatalf("truncate returned %d bytes, want <= 200", len(got))
	}
	if !strings.Contains(got, "newest") {
		t.Error("keepTail dropped the end of the input")
	}
	if strings.Contains(got, "oldest") {
		t.Error("keepTail kept the start of the input")
	}

	// Objects: the identifying metadata is at the start.
	obj := "apiVersion: v1\nkind: Pod\n" + strings.Repeat("  key: value\n", 200) + "trailer\n"
	got = truncate(obj, 200, keepHead)
	if len(got) > 200 {
		t.Fatalf("truncate returned %d bytes, want <= 200", len(got))
	}
	if !strings.Contains(got, "kind: Pod") {
		t.Error("keepHead dropped the start of the input")
	}
	if strings.Contains(got, "trailer") {
		t.Error("keepHead kept the end of the input")
	}
}

func TestTruncateAnnouncesItself(t *testing.T) {
	// A model that is not told its input was cut will report the missing part
	// as absent from the cluster.
	if got := truncate(strings.Repeat("x", 500), 100, keepHead); !strings.Contains(got, "truncated") {
		t.Error("truncated output does not say so")
	}
	if got := truncate("short", 100, keepHead); got != "short" {
		t.Errorf("truncate altered output that was within the limit: %q", got)
	}
}

func TestReadTailKeepsTheEnd(t *testing.T) {
	src := strings.NewReader(strings.Repeat("old\n", 5000) + "final line\n")

	data, dropped, err := readTail(src, 128)
	if err != nil {
		t.Fatalf("readTail: %v", err)
	}
	if !dropped {
		t.Error("dropped = false, want true for an oversized log")
	}
	if len(data) > 128 {
		t.Fatalf("readTail returned %d bytes, want <= 128", len(data))
	}
	if !strings.Contains(string(data), "final line") {
		t.Error("readTail dropped the tail of the log")
	}
	if strings.HasPrefix(string(data), "ld\n") {
		t.Error("readTail returned a partial first line")
	}
}

func TestReadTailShortInput(t *testing.T) {
	data, dropped, err := readTail(strings.NewReader("one\ntwo\n"), 4096)
	if err != nil {
		t.Fatalf("readTail: %v", err)
	}
	if dropped {
		t.Error("dropped = true for a log that fits")
	}
	if string(data) != "one\ntwo\n" {
		t.Errorf("readTail = %q, want the input unchanged", data)
	}
}

func TestRenderTable(t *testing.T) {
	table := &metav1.Table{
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "Name", Priority: 0},
			{Name: "Ready", Priority: 0},
			{Name: "Restarts", Priority: 0},
			{Name: "IP", Priority: 1}, // wide-only: kubectl hides it, so do we
		},
		Rows: []metav1.TableRow{
			{
				Cells:  []any{"web-0", "1/1", float64(3), "10.0.0.1"},
				Object: runtime.RawExtension{Raw: []byte(`{"metadata":{"namespace":"prod"}}`)},
			},
			{
				Cells:  []any{"web-1", "0/1", nil, "10.0.0.2"},
				Object: runtime.RawExtension{Raw: []byte(`{"metadata":{"namespace":"staging"}}`)},
			},
		},
	}

	out := renderTable(table, true)

	if !strings.Contains(out, "NAMESPACE") || !strings.Contains(out, "prod") {
		t.Errorf("cross-namespace listing lost the namespace column:\n%s", out)
	}
	if strings.Contains(out, "10.0.0.1") {
		t.Errorf("priority>0 column was rendered:\n%s", out)
	}
	// A restart count must not print as 3.000000: json numbers are float64.
	if !strings.Contains(out, "3") || strings.Contains(out, "3.0") {
		t.Errorf("integer cell rendered as a float:\n%s", out)
	}
	if !strings.Contains(out, "<none>") {
		t.Errorf("nil cell not rendered as <none>:\n%s", out)
	}

	if out := renderTable(table, false); strings.Contains(out, "NAMESPACE") {
		t.Errorf("single-namespace listing added a redundant namespace column:\n%s", out)
	}
}

func TestStripNoise(t *testing.T) {
	raw := []byte(`{
	  "kind": "Pod",
	  "metadata": {
	    "name": "web-0",
	    "managedFields": [{"manager": "kubectl"}],
	    "annotations": {"kubectl.kubernetes.io/last-applied-configuration": "{...}"}
	  }
	}`)

	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	stripNoise(obj)

	md := obj["metadata"].(map[string]any)
	if _, ok := md["managedFields"]; ok {
		t.Error("managedFields survived stripNoise")
	}
	if _, ok := md["annotations"]; ok {
		t.Error("an annotations map left empty by stripping was kept")
	}
	if md["name"] != "web-0" {
		t.Error("stripNoise removed a field that identifies the object")
	}
}
