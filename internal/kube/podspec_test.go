package kube

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestPreparePodSpecShareProcessNamespace(t *testing.T) {
	spec, err := PreparePodSpec(corev1.PodSpec{}, nil, "/workspace", "ghcr.io/acmore/okdev-sidecar:edge", corev1.ResourceRequirements{}, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if spec.ShareProcessNamespace == nil || !*spec.ShareProcessNamespace {
		t.Fatal("expected ShareProcessNamespace=true")
	}
}

func TestPreparePodSpecWorkspaceDefaultsToEmptyDir(t *testing.T) {
	spec, err := PreparePodSpec(corev1.PodSpec{}, nil, "/workspace", "ghcr.io/acmore/okdev-sidecar:edge", corev1.ResourceRequirements{}, false, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range spec.Volumes {
		if v.Name == "workspace" {
			if v.EmptyDir == nil {
				t.Fatal("expected workspace emptyDir when no workspace volume configured")
			}
			return
		}
	}
	t.Fatal("workspace volume not found")
}

func TestPreparePodSpecUsesConfiguredWorkspaceVolume(t *testing.T) {
	volumes := []corev1.Volume{
		{
			Name: "workspace",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: "team-workspace",
				},
			},
		},
	}
	spec, err := PreparePodSpec(corev1.PodSpec{}, volumes, "/workspace", "ghcr.io/acmore/okdev-sidecar:edge", corev1.ResourceRequirements{}, false, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range spec.Volumes {
		if v.Name == "workspace" {
			if v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName != "team-workspace" {
				t.Fatalf("expected workspace pvc claim team-workspace, got %#v", v.PersistentVolumeClaim)
			}
			return
		}
	}
	t.Fatal("workspace volume not found")
}

func TestPreparePodSpecAddsWorkspaceMountOnDevAndSidecar(t *testing.T) {
	spec, err := PreparePodSpec(corev1.PodSpec{}, nil, "/workspace", "ghcr.io/acmore/okdev-sidecar:edge", corev1.ResourceRequirements{}, false, "")
	if err != nil {
		t.Fatal(err)
	}
	var dev, sidecar corev1.Container
	for _, c := range spec.Containers {
		if c.Name == "dev" {
			dev = c
		}
		if c.Name == "okdev-sidecar" {
			sidecar = c
		}
	}
	hasMount := func(c corev1.Container, name, path string) bool {
		for _, m := range c.VolumeMounts {
			if m.Name == name && m.MountPath == path {
				return true
			}
		}
		return false
	}
	if !hasMount(dev, "workspace", "/workspace") {
		t.Fatal("expected workspace mount on dev")
	}
	if !hasMount(sidecar, "workspace", "/workspace") {
		t.Fatal("expected workspace mount on sidecar")
	}
	if !hasMount(dev, "okdev-runtime", "/var/okdev") {
		t.Fatal("expected okdev runtime mount on dev")
	}
	if !hasMount(sidecar, "okdev-runtime", "/var/okdev") {
		t.Fatal("expected okdev runtime mount on sidecar")
	}
}

func TestPreparePodSpecMirrorsTargetWorkspaceSubPathOnSidecar(t *testing.T) {
	spec, err := PreparePodSpecForTarget(corev1.PodSpec{
		Containers: []corev1.Container{
			{
				Name:  "pytorch",
				Image: "ubuntu",
				VolumeMounts: []corev1.VolumeMount{
					{
						Name:      "workspace",
						MountPath: "/workspace",
						SubPath:   "users/e2e/workspace",
					},
				},
			},
		},
	}, nil, "/workspace", "ghcr.io/acmore/okdev-sidecar:edge", corev1.ResourceRequirements{}, false, "", "pytorch")
	if err != nil {
		t.Fatal(err)
	}
	sidecar := findContainer(spec.Containers, "okdev-sidecar")
	if sidecar == nil {
		t.Fatal("sidecar container not found")
	}
	for _, mount := range sidecar.VolumeMounts {
		if mount.Name == "workspace" {
			if mount.MountPath != "/workspace" || mount.SubPath != "users/e2e/workspace" {
				t.Fatalf("expected sidecar workspace mount to mirror target subPath, got %+v", mount)
			}
			return
		}
	}
	t.Fatalf("expected sidecar workspace mount, got %+v", sidecar.VolumeMounts)
}

func TestPreparePodSpecErrorsOnEmptyImage(t *testing.T) {
	if _, err := PreparePodSpec(corev1.PodSpec{}, nil, "/workspace", "", corev1.ResourceRequirements{}, false, ""); err == nil {
		t.Fatal("expected error for empty sidecar image")
	}
}

func TestSyncthingImagePullPolicy(t *testing.T) {
	cases := []struct {
		image string
		want  corev1.PullPolicy
	}{
		{image: "ghcr.io/acmore/okdev:edge", want: corev1.PullAlways},
		{image: "ghcr.io/acmore/okdev:latest", want: corev1.PullAlways},
		{image: "ghcr.io/acmore/okdev", want: corev1.PullAlways},
		{image: "ghcr.io/acmore/okdev:v0.1.0", want: corev1.PullIfNotPresent},
		{image: "ghcr.io/acmore/okdev:1.2.3", want: corev1.PullIfNotPresent},
		{image: "ghcr.io/acmore/okdev@sha256:deadbeef", want: corev1.PullIfNotPresent},
	}
	for _, tc := range cases {
		if got := syncthingImagePullPolicy(tc.image); got != tc.want {
			t.Fatalf("image %q: want %s, got %s", tc.image, tc.want, got)
		}
	}
}

func findContainer(containers []corev1.Container, name string) *corev1.Container {
	for i := range containers {
		if containers[i].Name == name {
			return &containers[i]
		}
	}
	return nil
}

func TestPreparePodSpecSetsDevTmuxEnvWhenEnabled(t *testing.T) {
	spec, err := PreparePodSpec(corev1.PodSpec{}, nil, "/workspace", "ghcr.io/acmore/okdev-sidecar:edge", corev1.ResourceRequirements{}, true, "")
	if err != nil {
		t.Fatal(err)
	}
	dev := findContainer(spec.Containers, "dev")
	if dev == nil {
		t.Fatal("dev container not found")
	}
	for _, env := range dev.Env {
		if env.Name == "OKDEV_TMUX" && env.Value == "1" {
			return
		}
	}
	t.Fatal("expected OKDEV_TMUX=1 on dev container when tmux is enabled")
}

func TestPreparePodSpecForTargetUsesNamedContainer(t *testing.T) {
	spec, err := PreparePodSpecForTarget(corev1.PodSpec{
		Containers: []corev1.Container{
			{Name: "trainer", Image: "python:3.12"},
			{Name: "helper", Image: "busybox"},
		},
	}, nil, "/workspace", "ghcr.io/acmore/okdev-sidecar:edge", corev1.ResourceRequirements{}, false, "echo bye", "trainer")
	if err != nil {
		t.Fatal(err)
	}
	trainer := findContainer(spec.Containers, "trainer")
	if trainer == nil {
		t.Fatal("trainer container not found")
	}
	helper := findContainer(spec.Containers, "helper")
	if helper == nil {
		t.Fatal("helper container not found")
	}
	hasMount := func(c *corev1.Container, name string) bool {
		for _, m := range c.VolumeMounts {
			if m.Name == name {
				return true
			}
		}
		return false
	}
	if !hasMount(trainer, "workspace") || !hasMount(trainer, "okdev-runtime") {
		t.Fatalf("expected mounts on trainer, got %+v", trainer.VolumeMounts)
	}
	if hasMount(helper, "workspace") || hasMount(helper, "okdev-runtime") {
		t.Fatalf("did not expect runtime mounts on helper, got %+v", helper.VolumeMounts)
	}
	if trainer.Lifecycle == nil || trainer.Lifecycle.PreStop == nil {
		t.Fatal("expected prestop lifecycle on trainer container")
	}
}

func TestInjectPreStopForTargetUsesNamedContainer(t *testing.T) {
	spec := corev1.PodSpec{
		Containers: []corev1.Container{
			{Name: "trainer", Image: "python:3.12"},
			{Name: "helper", Image: "busybox"},
		},
	}
	InjectPreStopForTarget(&spec, "echo bye", "trainer")
	trainer := findContainer(spec.Containers, "trainer")
	helper := findContainer(spec.Containers, "helper")
	if trainer == nil || helper == nil {
		t.Fatal("expected both containers to exist")
	}
	if trainer.Lifecycle == nil || trainer.Lifecycle.PreStop == nil {
		t.Fatal("expected prestop lifecycle on trainer container")
	}
	if helper.Lifecycle != nil && helper.Lifecycle.PreStop != nil {
		t.Fatal("did not expect prestop lifecycle on helper container")
	}
}

func TestPreparePodSpecDefaultsWorkspacePathAndTargetContainer(t *testing.T) {
	spec, err := PreparePodSpecForTarget(corev1.PodSpec{
		Containers: []corev1.Container{
			{Name: "dev", Image: "ubuntu"},
		},
	}, nil, "", "ghcr.io/acmore/okdev-sidecar:edge", corev1.ResourceRequirements{}, false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	dev := findContainer(spec.Containers, "dev")
	if dev == nil {
		t.Fatal("dev container not found")
	}
	foundWorkspace := false
	for _, mount := range dev.VolumeMounts {
		if mount.Name == "workspace" && mount.MountPath == "/workspace" {
			foundWorkspace = true
		}
	}
	if !foundWorkspace {
		t.Fatalf("expected default workspace mount, got %+v", dev.VolumeMounts)
	}
}

func TestPreparePodSpecFallsBackToFirstContainerWhenTargetMissing(t *testing.T) {
	spec, err := PreparePodSpecForTarget(corev1.PodSpec{
		Containers: []corev1.Container{
			{Name: "first", Image: "ubuntu"},
			{Name: "second", Image: "ubuntu"},
		},
	}, nil, "/workspace", "ghcr.io/acmore/okdev-sidecar:edge", corev1.ResourceRequirements{}, false, "", "missing")
	if err != nil {
		t.Fatal(err)
	}
	first := findContainer(spec.Containers, "first")
	second := findContainer(spec.Containers, "second")
	if first == nil || second == nil {
		t.Fatal("expected both containers to exist")
	}
	if len(first.VolumeMounts) == 0 {
		t.Fatal("expected first container to receive mounts")
	}
	if len(second.VolumeMounts) != 0 {
		t.Fatalf("did not expect second container mounts, got %+v", second.VolumeMounts)
	}
}

func TestPreparePodSpecDoesNotDuplicateExistingSidecar(t *testing.T) {
	spec, err := PreparePodSpec(corev1.PodSpec{
		Containers: []corev1.Container{
			{Name: "dev", Image: "ubuntu"},
			{Name: "okdev-sidecar", Image: "existing"},
		},
	}, nil, "/workspace", "ghcr.io/acmore/okdev-sidecar:edge", corev1.ResourceRequirements{}, false, "")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, c := range spec.Containers {
		if c.Name == "okdev-sidecar" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected one sidecar container, got %d", count)
	}
}

func TestPreparePodSpecAppliesSidecarResources(t *testing.T) {
	spec, err := PreparePodSpec(corev1.PodSpec{}, nil, "/workspace", "ghcr.io/acmore/okdev-sidecar:edge", corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("250m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("250m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}, false, "")
	if err != nil {
		t.Fatal(err)
	}
	sidecar := findContainer(spec.Containers, "okdev-sidecar")
	if sidecar == nil {
		t.Fatal("sidecar container not found")
	}
	if got := sidecar.Resources.Requests.Cpu().String(); got != "250m" {
		t.Fatalf("unexpected cpu request %q", got)
	}
	if got := sidecar.Resources.Requests.Memory().String(); got != "512Mi" {
		t.Fatalf("unexpected memory request %q", got)
	}
	if got := sidecar.Resources.Limits.Cpu().String(); got != "250m" {
		t.Fatalf("unexpected cpu limit %q", got)
	}
}

func TestEnsureHelpers(t *testing.T) {
	volumes := ensureVolume([]corev1.Volume{{Name: "existing"}}, corev1.Volume{Name: "existing"})
	if len(volumes) != 1 {
		t.Fatalf("expected no duplicate volume, got %d", len(volumes))
	}

	mounts := ensureVolumeMount([]corev1.VolumeMount{{Name: "workspace"}}, corev1.VolumeMount{Name: "workspace", MountPath: "/workspace"})
	if mounts[0].MountPath != "/workspace" {
		t.Fatalf("expected missing mount path to be filled, got %+v", mounts[0])
	}
	mounts = ensureVolumeMount([]corev1.VolumeMount{{Name: "workspace", MountPath: "/custom"}}, corev1.VolumeMount{Name: "workspace", MountPath: "/workspace"})
	if mounts[0].MountPath != "/custom" {
		t.Fatalf("expected existing mount path to win, got %+v", mounts[0])
	}

	envs := ensureEnvVar([]corev1.EnvVar{{Name: "OKDEV_WORKSPACE"}}, corev1.EnvVar{Name: "OKDEV_WORKSPACE", Value: "/workspace"})
	if envs[0].Value != "/workspace" {
		t.Fatalf("expected empty env value to be filled, got %+v", envs[0])
	}
	envs = ensureEnvVar([]corev1.EnvVar{{Name: "OKDEV_WORKSPACE", Value: "/custom"}}, corev1.EnvVar{Name: "OKDEV_WORKSPACE", Value: "/workspace"})
	if envs[0].Value != "/custom" {
		t.Fatalf("expected existing env value to win, got %+v", envs[0])
	}

	if !hasContainer([]corev1.Container{{Name: "dev"}}, "dev") {
		t.Fatal("expected container lookup success")
	}
	if hasContainer([]corev1.Container{{Name: "dev"}}, "sidecar") {
		t.Fatal("did not expect missing container to be reported present")
	}
}

func TestImageTag(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/acmore/okdev:edge":            "edge",
		"ghcr.io/acmore/okdev":                 "",
		"localhost:5000/okdev:latest":          "latest",
		"ghcr.io/acmore/okdev@sha256:deadbeef": "deadbeef",
		"ghcr.io/acmore/okdev:1.2.3-alpine":    "1.2.3-alpine",
	}
	for image, want := range cases {
		if got := imageTag(image); got != want {
			t.Fatalf("image %q: got %q want %q", image, got, want)
		}
	}
}
