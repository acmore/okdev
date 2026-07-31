package config

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
)

const (
	SnapshotVersion           = "v1"
	AnnotationLastAppliedSpec = "okdev.io/last-applied-spec"
	AnnotationLastAppliedHash = "okdev.io/last-applied-spec-sha256"
)

type LastAppliedWorkloadSpec struct {
	Version            string                      `json:"version"`
	WorkloadKind       string                      `json:"workloadKind"`
	Workload           WorkloadSpec                `json:"workload"`
	SidecarImage       string                      `json:"sidecarImage"`
	SidecarResources   corev1.ResourceRequirements `json:"sidecarResources,omitempty"`
	WorkspaceMountPath string                      `json:"workspaceMountPath"`
	TargetContainer    string                      `json:"targetContainer"`
	Tmux               bool                        `json:"tmux"`
	Shell              string                      `json:"shell,omitempty"`
	PreStop            string                      `json:"preStop"`
	ManifestPath       string                      `json:"manifestPath,omitempty"`
	ManifestSHA256     string                      `json:"manifestSHA256,omitempty"`
	// WorkloadProfile distinguishes two profiles that share a type (a one-GPU
	// "dev" pod and an eight-GPU "big" pod produce otherwise identical
	// snapshots). It is stamped only for configs that declare spec.workloads:
	// omitting it for desugared legacy configs keeps their hash bit-for-bit
	// what it was before profiles existed, so existing sessions do not all
	// report drift on their next `okdev up`.
	WorkloadProfile string `json:"workloadProfile,omitempty"`
	// SyncRemoteRoots are the remote roots of additional sync mappings.
	// They shape the pod spec (auto-provisioned volumes + sidecar mounts),
	// so adding or removing a mapping must surface as workload drift and
	// prompt a reconcile. omitempty keeps single-mapping sessions on the
	// legacy snapshot hash.
	SyncRemoteRoots []string `json:"syncRemoteRoots,omitempty"`
}

func BuildWorkloadSnapshot(cfg *DevEnvironment, workspaceMountPath, targetContainer string, tmux bool, shell string, preStop, manifestPath, manifestResolvedPath string) LastAppliedWorkloadSpec {
	kind := cfg.Spec.Workload.Type
	if kind == "" {
		kind = "pod"
	}
	workloadSpec := cfg.Spec.Workload
	// Only a configured inject enters the snapshot. A pod's root inject path is
	// synthesized at apply time and does not change the resulting object, so
	// recording it would shift the hash of every pod session created before it
	// existed — turning a plain `okdev up` into a demand to recreate the
	// workload. Job/generic/pytorchjob always have a configured inject (set by
	// the user or by SetDefaults), so they are unaffected.
	if len(cfg.Spec.Workload.Inject) > 0 {
		workloadSpec.Inject = cfg.EffectiveWorkloadInject()
	}
	snap := LastAppliedWorkloadSpec{
		Version:            SnapshotVersion,
		WorkloadKind:       kind,
		Workload:           workloadSpec,
		SidecarImage:       cfg.Spec.Sidecar.Image,
		SidecarResources:   cfg.Spec.Sidecar.Resources,
		WorkspaceMountPath: workspaceMountPath,
		TargetContainer:    targetContainer,
		Tmux:               tmux,
		Shell:              shell,
		PreStop:            preStop,
		ManifestPath:       manifestPath,
		SyncRemoteRoots:    cfg.AdditionalSyncRemoteRoots(),
	}
	if cfg.DeclaresWorkloadProfiles() {
		snap.WorkloadProfile = cfg.SelectedWorkload()
	}
	if manifestResolvedPath != "" {
		hash, err := ComputeManifestSHA256(manifestResolvedPath)
		if err == nil {
			snap.ManifestSHA256 = hash
		}
	}
	return snap
}

func (s *LastAppliedWorkloadSpec) JSON() (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("marshal last-applied spec: %w", err)
	}
	return string(b), nil
}

func (s *LastAppliedWorkloadSpec) SHA256() (string, error) {
	hashInput := *s
	hashInput.ManifestPath = ""
	b, err := json.Marshal(hashInput)
	if err != nil {
		return "", fmt.Errorf("marshal for hash: %w", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(b)), nil
}

func ParseLastAppliedWorkloadSpec(raw string) (LastAppliedWorkloadSpec, error) {
	var snap LastAppliedWorkloadSpec
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		return LastAppliedWorkloadSpec{}, fmt.Errorf("parse last-applied spec: %w", err)
	}
	return snap, nil
}

func ComputeManifestSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read manifest %q: %w", path, err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}
