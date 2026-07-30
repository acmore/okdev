package workload

import "testing"

func TestResolveMapPathRootReturnsObjectItself(t *testing.T) {
	root := map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"spec":       map[string]any{"containers": []any{}},
	}
	got, err := resolveMapPath(root, "")
	if err != nil {
		t.Fatalf("resolveMapPath: %v", err)
	}
	if got["kind"] != "Pod" {
		t.Fatalf("expected the root map, got %v", got)
	}
}

func TestWriteMapPathRootMergesAndPreservesTypeMeta(t *testing.T) {
	root := map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]any{"name": "old"},
		"spec":       map[string]any{"containers": []any{}},
	}
	value := map[string]any{
		"metadata": map[string]any{"name": "new", "labels": map[string]any{"a": "b"}},
		"spec":     map[string]any{"containers": []any{"c"}},
	}
	if err := writeMapPath(root, "", value); err != nil {
		t.Fatalf("writeMapPath: %v", err)
	}
	if root["apiVersion"] != "v1" || root["kind"] != "Pod" {
		t.Fatalf("type meta was lost: %v", root)
	}
	if _, bad := root[""]; bad {
		t.Fatal(`writeMapPath created an empty-string key`)
	}
	meta, _ := root["metadata"].(map[string]any)
	if meta["name"] != "new" {
		t.Fatalf("metadata not merged: %v", root["metadata"])
	}
	spec, _ := root["spec"].(map[string]any)
	if len(spec["containers"].([]any)) != 1 {
		t.Fatalf("spec not merged: %v", root["spec"])
	}
}
