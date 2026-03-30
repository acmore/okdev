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
	Version            string          `json:"version"`
	WorkloadKind       string          `json:"workloadKind"`
	Workload           WorkloadSpec    `json:"workload"`
	PodTemplate        PodTemplateRef  `json:"podTemplate"`
	Volumes            []corev1.Volume `json:"volumes"`
	SidecarImage       string          `json:"sidecarImage"`
	WorkspaceMountPath string          `json:"workspaceMountPath"`
	TargetContainer    string          `json:"targetContainer"`
	Tmux               bool            `json:"tmux"`
	PreStop            string          `json:"preStop"`
	ManifestPath       string          `json:"manifestPath,omitempty"`
	ManifestSHA256     string          `json:"manifestSHA256,omitempty"`
}

func BuildWorkloadSnapshot(cfg *DevEnvironment, workspaceMountPath, targetContainer string, tmux bool, preStop, manifestPath, manifestResolvedPath string) LastAppliedWorkloadSpec {
	kind := cfg.Spec.Workload.Type
	if kind == "" {
		kind = "pod"
	}
	snap := LastAppliedWorkloadSpec{
		Version:            SnapshotVersion,
		WorkloadKind:       kind,
		Workload:           cfg.Spec.Workload,
		PodTemplate:        cfg.Spec.PodTemplate,
		Volumes:            cfg.Spec.Volumes,
		SidecarImage:       cfg.Spec.Sidecar.Image,
		WorkspaceMountPath: workspaceMountPath,
		TargetContainer:    targetContainer,
		Tmux:               tmux,
		PreStop:            preStop,
		ManifestPath:       manifestPath,
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
