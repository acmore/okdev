package workload

import (
	"fmt"
	"math"
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type ReplicaExpectation struct {
	Role  string `json:"role"`
	Count int    `json:"declared"`
}

// DeclaredReplicas reads the rendered source manifest, without applying it.
// Batch Jobs and unknown controllers have no generic steady-state pod count.
func (r *GenericRuntime) DeclaredReplicas() (string, string, []ReplicaExpectation, error) {
	obj, err := r.load()
	if err != nil {
		return "", "", nil, err
	}
	kind, name := obj.GetKind(), obj.GetName()
	var groups []ReplicaExpectation
	switch obj.GetAPIVersion() + "/" + kind {
	case "apps/v1/Deployment", "apps/v1/StatefulSet":
		n, err := replicaCount(obj.Object, "spec", "replicas")
		if err != nil {
			return kind, name, nil, err
		}
		groups = append(groups, ReplicaExpectation{Role: "pods", Count: n})
	case "kubeflow.org/v1/PyTorchJob":
		specs, found, err := unstructured.NestedMap(obj.Object, "spec", "pytorchReplicaSpecs")
		if err != nil || !found {
			return kind, name, nil, fmt.Errorf("missing or invalid pytorchReplicaSpecs")
		}
		roles := make([]string, 0, len(specs))
		for role := range specs {
			roles = append(roles, role)
		}
		sort.Strings(roles)
		for _, role := range roles {
			n, err := replicaCount(obj.Object, "spec", "pytorchReplicaSpecs", role, "replicas")
			if err != nil {
				return kind, name, nil, err
			}
			groups = append(groups, ReplicaExpectation{Role: role, Count: n})
		}
	}
	return kind, name, groups, nil
}

func replicaCount(obj map[string]any, path ...string) (int, error) {
	value, found, err := unstructured.NestedFieldNoCopy(obj, path...)
	if err != nil {
		return 0, err
	}
	if !found {
		return 1, nil
	}
	var n float64
	switch v := value.(type) {
	case float64:
		n = v
	case int64:
		n = float64(v)
	default:
		return 0, fmt.Errorf("invalid replica count %v", value)
	}
	if n < 0 || n > math.MaxInt32 || math.Trunc(n) != n {
		return 0, fmt.Errorf("invalid replica count %v", value)
	}
	return int(n), nil
}
