package workload

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDeclaredReplicas(t *testing.T) {
	for _, tc := range []struct {
		name, api, kind, spec string
		want                  []ReplicaExpectation
		bad                   bool
	}{
		{"deployment default", "apps/v1", "Deployment", `{}`, []ReplicaExpectation{{"pods", 1}}, false},
		{"zero", "apps/v1", "StatefulSet", `{"replicas":0}`, []ReplicaExpectation{{"pods", 0}}, false},
		{"roles", "kubeflow.org/v1", "PyTorchJob", `{"pytorchReplicaSpecs":{"Worker":{"replicas":4},"Master":{}}}`, []ReplicaExpectation{{"Master", 1}, {"Worker", 4}}, false},
		{"negative", "apps/v1", "Deployment", `{"replicas":-1}`, nil, true},
		{"fraction", "apps/v1", "Deployment", `{"replicas":1.5}`, nil, true},
		{"string", "apps/v1", "Deployment", `{"replicas":"4"}`, nil, true},
		{"batch is not steady state", "batch/v1", "Job", `{"parallelism":4}`, nil, false},
		{"unknown CRD", "other/v1", "Deployment", `{"replicas":4}`, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "workload.json")
			raw := []byte(`{"apiVersion":"` + tc.api + `","kind":"` + tc.kind + `","metadata":{"name":"{{ .WorkloadName }}"},"spec":` + tc.spec + `}`)
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			rt := &GenericRuntime{ManifestPath: path, WorkloadNameOverride: "rendered"}
			kind, name, groups, err := rt.DeclaredReplicas()
			if (err != nil) != tc.bad {
				t.Fatalf("error=%v", err)
			}
			if tc.bad {
				return
			}
			if kind != tc.kind || name != "rendered" || len(groups) != len(tc.want) {
				t.Fatalf("%s %s %+v", kind, name, groups)
			}
			for i, want := range tc.want {
				if groups[i] != want {
					t.Fatalf("got %+v want %+v", groups, want)
				}
			}
			after, _ := os.ReadFile(path)
			if string(after) != string(raw) {
				t.Fatal("manifest modified")
			}
		})
	}
}
