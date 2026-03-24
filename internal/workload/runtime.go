package workload

import (
	"context"
	"time"

	"github.com/acmore/okdev/internal/kube"
)

const (
	TypePod        = "pod"
	TypeJob        = "job"
	TypePyTorchJob = "pytorchjob"
	TypeGeneric    = "generic"
)

type TargetRef struct {
	PodName   string `json:"podName"`
	Container string `json:"container"`
	Role      string `json:"role,omitempty"`
}

type Runtime interface {
	Kind() string
	WorkloadName() string
	Apply(ctx context.Context, k ApplyClient, namespace string) error
	Delete(ctx context.Context, k DeleteClient, namespace string, ignoreNotFound bool) error
	WaitReady(ctx context.Context, k WaitClient, namespace string, timeout time.Duration, onProgress func(kube.PodReadinessProgress)) error
	SelectTarget(ctx context.Context, k TargetClient, namespace string) (TargetRef, error)
}

type ApplyClient interface {
	Apply(ctx context.Context, namespace string, manifest []byte) error
}

type DeleteClient interface {
	Delete(ctx context.Context, namespace string, kind string, name string, ignoreNotFound bool) error
	DeleteByRef(ctx context.Context, namespace string, apiVersion string, kind string, name string, ignoreNotFound bool) error
}

type WaitClient interface {
	WaitReadyWithProgress(ctx context.Context, namespace, pod string, timeout time.Duration, onProgress func(kube.PodReadinessProgress)) error
	ListPods(ctx context.Context, namespace string, allNamespaces bool, labelSelector string) ([]kube.PodSummary, error)
}

type TargetClient interface {
	GetPodSummary(ctx context.Context, namespace, name string) (*kube.PodSummary, error)
	ListPods(ctx context.Context, namespace string, allNamespaces bool, labelSelector string) ([]kube.PodSummary, error)
}
