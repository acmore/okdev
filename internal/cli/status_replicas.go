package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/session"
	"github.com/acmore/okdev/internal/workload"
)

type replicaStatus struct {
	Source      string               `json:"source"`
	Kind        string               `json:"kind,omitempty"`
	Name        string               `json:"name,omitempty"`
	Groups      []replicaGroupStatus `json:"groups,omitempty"`
	Unavailable string               `json:"unavailable,omitempty"`
}

type replicaGroupStatus struct {
	workload.ReplicaExpectation
	Present       int  `json:"present"`
	Running       int  `json:"running"`
	Ready         int  `json:"ready"`
	CountMismatch bool `json:"countMismatch"`
}

func buildReplicaStatus(cfg *config.DevEnvironment, cfgPath string, view sessionView) *replicaStatus {
	if cfg == nil || cfg.Spec.Workload.ManifestPath == "" {
		return nil
	}
	info, _ := session.LoadInfo(view.Session)
	name, runID := info.WorkloadName, info.RunID
	for _, pod := range view.Pods {
		if v := pod.Labels["okdev.io/workload-name"]; v != "" {
			name = v
		}
		if v := pod.Labels["okdev.io/run-id"]; v != "" {
			runID = v
		}
		if name != "" {
			break
		}
	}
	rt, err := sessionRuntime(cfg, cfgPath, view.Session, name, discoveryLabelsForSession(cfg, view.Session, runID), nil, false, "")
	result := &replicaStatus{Source: workload.ResolveManifestPath(cfgPath, cfg.Spec.Workload.ManifestPath)}
	if err != nil {
		result.Unavailable = err.Error()
		return result
	}
	generic, ok := rt.(*workload.GenericRuntime)
	if !ok {
		return nil
	}
	kind, name, groups, err := generic.DeclaredReplicas()
	if err != nil {
		result.Unavailable = err.Error()
		return result
	}
	if len(groups) == 0 {
		return nil
	}
	result.Kind, result.Name = kind, name
	for _, expected := range groups {
		row := replicaGroupStatus{ReplicaExpectation: expected}
		for _, pod := range view.Pods {
			if pod.Deleting || pod.Phase == "Terminating" || pod.Phase == "Succeeded" || pod.Phase == "Failed" {
				continue
			}
			if pod.Labels["okdev.io/workload-name"] != name || pod.Labels["okdev.io/workload-resource-kind"] != kind {
				continue
			}
			if kind == "PyTorchJob" && !strings.EqualFold(pod.Labels["training.kubeflow.org/replica-type"], expected.Role) {
				continue
			}
			row.Present++
			if pod.Phase == "Running" {
				row.Running++
				ready, total, ok := strings.Cut(pod.Ready, "/")
				count, _ := strconv.Atoi(total)
				if ok && count > 0 && ready == total {
					row.Ready++
				}
			}
		}
		row.CountMismatch = row.Present != expected.Count
		result.Groups = append(result.Groups, row)
	}
	return result
}

func printReplicaStatus(w io.Writer, status *replicaStatus) {
	if status == nil {
		return
	}
	if status.Unavailable != "" {
		fmt.Fprintf(w, "replicas: comparison unavailable (%s)\n", status.Unavailable)
		return
	}
	for _, row := range status.Groups {
		note := ""
		if row.CountMismatch {
			note = "; count differs from manifest (may be scaling or rollout)"
		}
		fmt.Fprintf(w, "replicas %s/%s %s: manifest declares %d; %d present, %d running, %d ready%s\n", status.Kind, status.Name, row.Role, row.Count, row.Present, row.Running, row.Ready, note)
	}
}
