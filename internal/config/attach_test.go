package config

import (
	"strings"
	"testing"
)

func TestAttachOnlyConfigBoundaries(t *testing.T) {
	base := "apiVersion: okdev.io/v1alpha1\nkind: DevEnvironment\nmetadata:\n  name: access\nspec:\n"
	for _, tc := range []struct {
		name, spec string
		valid      bool
	}{
		{"pods", "  attachOnly: {pods: [worker-0], container: app}\n", true},
		{"selector", "  attachOnly: {selector: 'app=external', container: app}\n", true},
		{"session", "  attachOnly: {session: existing, container: dev}\n", true},
		{"empty", "  attachOnly: {container: app}\n", false},
		{"ambiguous", "  attachOnly: {pods: [worker-0], selector: 'app=x', container: app}\n", false},
		{"container", "  attachOnly: {pods: [worker-0]}\n", false},
		{"selector syntax", "  attachOnly: {selector: 'app in (', container: app}\n", false},
		{"pod name", "  attachOnly: {pods: [''], container: app}\n", false},
		{"workload", "  attachOnly: {pods: [worker-0], container: app}\n  workload: {type: pod, manifestPath: pod.yaml}\n", false},
		{"gateway", "  attachOnly: {pods: [worker-0], container: app}\n  exec: {fanoutMode: gateway}\n", false},
		{"ordinary manifest still required", "  workload: {type: pod}\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, err := LoadFromBytes([]byte(base+tc.spec), "attach.yaml")
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
			if tc.valid && (len(cfg.Spec.Workloads) != 0 || strings.TrimSpace(cfg.Spec.Workload.Type) != "") {
				t.Fatal("attach config acquired a deployable profile")
			}
		})
	}
}
