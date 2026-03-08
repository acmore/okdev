# Lifecycle Hooks & .okdev/ Folder Convention Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add postCreate, postAttach, and preStop lifecycle hooks with .okdev/ folder convention for config and scripts.

**Architecture:** Config resolution updated to prefer `.okdev/okdev.yaml`. New `LifecycleSpec` in config. `postCreate` runs via kubectl exec after pod ready and sync, guarded by pod annotation. `postAttach` runs in sidecar's nsenter-dev.sh. `preStop` wired as Kubernetes lifecycle hook on the dev container.

**Tech Stack:** Go (config/CLI/kube), shell script (sidecar), Kubernetes lifecycle hooks

---

### Task 1: Update config resolution to support .okdev/ folder

**Files:**
- Modify: `internal/config/loader.go:12-14` (constants), `internal/config/loader.go:58` (discoverInParents call)
- Test: `internal/config/loader_test.go`

**Step 1: Write the failing test**

Add to `internal/config/loader_test.go`:

```go
func TestResolvePathPrefersFolderConfig(t *testing.T) {
	tmp := t.TempDir()
	okdevDir := filepath.Join(tmp, ".okdev")
	if err := os.MkdirAll(okdevDir, 0o755); err != nil {
		t.Fatal(err)
	}
	folderCfg := filepath.Join(okdevDir, "okdev.yaml")
	rootCfg := filepath.Join(tmp, DefaultFile)

	writeFile(t, folderCfg, validConfigYAML("folder"))
	writeFile(t, rootCfg, validConfigYAML("root"))

	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}

	p, err := ResolvePath("")
	if err != nil {
		t.Fatal(err)
	}
	assertSameFile(t, folderCfg, p)
}

func TestResolvePathFallsBackToRootConfig(t *testing.T) {
	tmp := t.TempDir()
	rootCfg := filepath.Join(tmp, DefaultFile)
	writeFile(t, rootCfg, validConfigYAML("root"))

	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}

	p, err := ResolvePath("")
	if err != nil {
		t.Fatal(err)
	}
	assertSameFile(t, rootCfg, p)
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestResolvePathPrefersFolderConfig -v`
Expected: FAIL (`.okdev/okdev.yaml` not in search order)

**Step 3: Update loader.go**

Add a new constant and update the `discoverInParents` call:

```go
const (
	DefaultFile = ".okdev.yaml"
	FolderFile  = ".okdev/okdev.yaml"
	LegacyFile  = "okdev.yaml"
)
```

Change line 58 from:
```go
p, err := discoverInParents(wd, gitRoot, DefaultFile, LegacyFile)
```
To:
```go
p, err := discoverInParents(wd, gitRoot, FolderFile, DefaultFile, LegacyFile)
```

**Step 4: Run tests to verify they pass**

Run: `go test ./internal/config/ -v`
Expected: ALL PASS

**Step 5: Commit**

```bash
git add internal/config/loader.go internal/config/loader_test.go
git commit -m "feat(config): support .okdev/okdev.yaml folder convention"
```

---

### Task 2: Add LifecycleSpec to config

**Files:**
- Modify: `internal/config/config.go:33-42` (DevEnvSpec struct)
- Test: `internal/config/config_test.go`

**Step 1: Write the failing test**

Add to `internal/config/config_test.go`:

```go
func TestLifecycleSpecParsed(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Lifecycle.PostCreate = "make setup"
	cfg.Spec.Lifecycle.PreStop = "make clean"
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	if cfg.Spec.Lifecycle.PostCreate != "make setup" {
		t.Fatalf("expected postCreate 'make setup', got %q", cfg.Spec.Lifecycle.PostCreate)
	}
	if cfg.Spec.Lifecycle.PreStop != "make clean" {
		t.Fatalf("expected preStop 'make clean', got %q", cfg.Spec.Lifecycle.PreStop)
	}
}

func TestLifecycleSpecEmpty(t *testing.T) {
	cfg := validConfig()
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	if cfg.Spec.Lifecycle.PostCreate != "" || cfg.Spec.Lifecycle.PreStop != "" {
		t.Fatal("expected empty lifecycle spec by default")
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestLifecycleSpec -v`
Expected: FAIL — `Lifecycle` field does not exist

**Step 3: Add LifecycleSpec struct and field**

In `internal/config/config.go`, add the struct after `PortMapping`:

```go
type LifecycleSpec struct {
	PostCreate string `yaml:"postCreate"`
	PreStop    string `yaml:"preStop"`
}
```

Add `Lifecycle` field to `DevEnvSpec`:

```go
type DevEnvSpec struct {
	Namespace   string         `yaml:"namespace"`
	Session     SessionSpec    `yaml:"session"`
	Workspace   Workspace      `yaml:"workspace"`
	Sync        SyncSpec       `yaml:"sync"`
	Ports       []PortMapping  `yaml:"ports"`
	SSH         SSHSpec        `yaml:"ssh"`
	Lifecycle   LifecycleSpec  `yaml:"lifecycle"`
	Sidecar     SidecarSpec    `yaml:"sidecar"`
	PodTemplate PodTemplateRef `yaml:"podTemplate"`
}
```

No changes to `SetDefaults` or `Validate` — empty strings are valid (convention-based discovery).

**Step 4: Run tests to verify they pass**

Run: `go test ./internal/config/ -v`
Expected: ALL PASS

**Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): add LifecycleSpec for postCreate and preStop hooks"
```

---

### Task 3: Add AnnotatePod method to kube client

**Files:**
- Modify: `internal/kube/client.go` (add method near `TouchPodActivity` at line 411)

**Step 1: Add the method**

Add after `TouchPodActivity` (around line 419):

```go
func (c *Client) AnnotatePod(ctx context.Context, namespace, pod, key, value string) error {
	cs, _, err := c.clientset()
	if err != nil {
		return err
	}
	patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, key, value))
	_, err = cs.CoreV1().Pods(namespace).Patch(ctx, pod, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

func (c *Client) GetPodAnnotation(ctx context.Context, namespace, pod, key string) (string, error) {
	cs, _, err := c.clientset()
	if err != nil {
		return "", err
	}
	p, err := cs.CoreV1().Pods(namespace).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return p.Annotations[key], nil
}
```

**Step 2: Build to verify**

Run: `go build ./internal/kube/`
Expected: Success

**Step 3: Commit**

```bash
git add internal/kube/client.go
git commit -m "feat(kube): add AnnotatePod and GetPodAnnotation methods"
```

---

### Task 4: Add preStop lifecycle hook to dev container in pod spec

**Files:**
- Modify: `internal/kube/podspec.go:13` (PreparePodSpec signature + dev container lifecycle)
- Test: `internal/kube/podspec_test.go`

**Step 1: Write the failing tests**

Add to `internal/kube/podspec_test.go`:

```go
func TestPreparePodSpecPreStopHook(t *testing.T) {
	spec, err := PreparePodSpec(corev1.PodSpec{}, "ws-pvc", "/workspace", "ghcr.io/acmore/okdev-sidecar:edge", false, "make clean")
	if err != nil {
		t.Fatal(err)
	}
	dev := spec.Containers[0]
	if dev.Lifecycle == nil || dev.Lifecycle.PreStop == nil {
		t.Fatal("expected preStop lifecycle hook on dev container")
	}
	if dev.Lifecycle.PreStop.Exec == nil {
		t.Fatal("expected exec action in preStop hook")
	}
	want := []string{"sh", "-c", "make clean"}
	for i, w := range want {
		if i >= len(dev.Lifecycle.PreStop.Exec.Command) || dev.Lifecycle.PreStop.Exec.Command[i] != w {
			t.Fatalf("expected preStop command %v, got %v", want, dev.Lifecycle.PreStop.Exec.Command)
		}
	}
}

func TestPreparePodSpecNoPreStopHook(t *testing.T) {
	spec, err := PreparePodSpec(corev1.PodSpec{}, "ws-pvc", "/workspace", "ghcr.io/acmore/okdev-sidecar:edge", false, "")
	if err != nil {
		t.Fatal(err)
	}
	dev := spec.Containers[0]
	if dev.Lifecycle != nil && dev.Lifecycle.PreStop != nil {
		t.Fatal("expected no preStop lifecycle hook when preStop is empty")
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/kube/ -run TestPreparePodSpecPreStop -v`
Expected: FAIL — too many arguments

**Step 3: Add `preStop` parameter to PreparePodSpec**

Update signature:
```go
func PreparePodSpec(podSpec corev1.PodSpec, workspaceClaim, workspaceMountPath, sidecarImage string, tmux bool, preStop string) (corev1.PodSpec, error) {
```

After the volume mount loop (after line 49), add preStop hook to the first container (dev container):

```go
	if preStop != "" && len(spec.Containers) > 0 {
		if spec.Containers[0].Lifecycle == nil {
			spec.Containers[0].Lifecycle = &corev1.Lifecycle{}
		}
		spec.Containers[0].Lifecycle.PreStop = &corev1.LifecycleHandler{
			Exec: &corev1.ExecAction{
				Command: []string{"sh", "-c", preStop},
			},
		}
	}
```

**Step 4: Update ALL existing callers and tests**

- `internal/cli/up.go` — update `kube.PreparePodSpec(...)` call to add `""` as the last arg (will be wired in Task 5)
- All existing tests in `internal/kube/podspec_test.go` — add `""` as the 6th argument to every existing `PreparePodSpec(...)` call

**Step 5: Run all tests**

Run: `go test ./internal/kube/ -v && go build ./internal/cli/`
Expected: ALL PASS

**Step 6: Commit**

```bash
git add internal/kube/podspec.go internal/kube/podspec_test.go internal/cli/up.go
git commit -m "feat(kube): add preStop lifecycle hook to dev container"
```

---

### Task 5: Wire postCreate and preStop in okdev up

**Files:**
- Modify: `internal/cli/up.go`

**Step 1: Add postCreate execution and preStop wiring**

In `up.go`, the postCreate should run **after sync starts** (so scripts are on the pod) but the annotation check should happen before execution.

Add a helper function in `up.go` (or a new file `internal/cli/lifecycle.go`):

```go
func resolvePostCreateCommand(cfg *config.DevEnvironment, configDir string) string {
	if cfg.Spec.Lifecycle.PostCreate != "" {
		return cfg.Spec.Lifecycle.PostCreate
	}
	// Convention: .okdev/post-create.sh
	script := filepath.Join(configDir, ".okdev", "post-create.sh")
	if _, err := os.Stat(script); err == nil {
		return filepath.Join(cfg.Spec.Workspace.MountPath, ".okdev", "post-create.sh")
	}
	return ""
}

func resolvePreStopCommand(cfg *config.DevEnvironment, configDir string) string {
	if cfg.Spec.Lifecycle.PreStop != "" {
		return cfg.Spec.Lifecycle.PreStop
	}
	script := filepath.Join(configDir, ".okdev", "pre-stop.sh")
	if _, err := os.Stat(script); err == nil {
		return filepath.Join(cfg.Spec.Workspace.MountPath, ".okdev", "pre-stop.sh")
	}
	return ""
}
```

In the `newUpCmd` RunE function:

1. Resolve configDir from the config path (directory of the loaded config file).

2. Wire preStop into PreparePodSpec:
```go
preStopCmd := resolvePreStopCommand(cfg, configDir)
preparedSpec, err := kube.PreparePodSpec(
    cfg.Spec.PodTemplate.Spec,
    pvc,
    cfg.Spec.Workspace.MountPath,
    cfg.Spec.Sidecar.Image,
    enableTmux,
    preStopCmd,
)
```

3. After syncthing sync starts (after line 148), add postCreate execution:
```go
// Run postCreate if not already done
postCreateCmd := resolvePostCreateCommand(cfg, configDir)
if postCreateCmd != "" {
    annotation, _ := k.GetPodAnnotation(ctx, ns, pod, "okdev.io/post-create-done")
    if annotation != "true" {
        fmt.Fprintf(cmd.OutOrStdout(), "Running postCreate: %s\n", postCreateCmd)
        if _, err := k.ExecShInContainer(ctx, ns, pod, "dev", postCreateCmd); err != nil {
            return fmt.Errorf("postCreate failed: %w", err)
        }
        if err := k.AnnotatePod(ctx, ns, pod, "okdev.io/post-create-done", "true"); err != nil {
            fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to annotate pod after postCreate: %v\n", err)
        }
        fmt.Fprintln(cmd.OutOrStdout(), "postCreate completed successfully")
    }
}
```

**Step 2: Build to verify**

Run: `go build ./internal/cli/`
Expected: Success

**Step 3: Commit**

```bash
git add internal/cli/up.go internal/cli/lifecycle.go
git commit -m "feat(cli): wire postCreate and preStop lifecycle hooks in okdev up"
```

---

### Task 6: Add postAttach to sidecar entrypoint

**Files:**
- Modify: `infra/sidecar/entrypoint.sh` (nsenter-dev.sh heredoc, interactive shell section)

**Step 1: Add postAttach before interactive shell**

In the `nsenter-dev.sh` script inside `entrypoint.sh`, add before the interactive shell section (before line 65 "# Interactive login shell"):

```bash
# Run postAttach script if it exists in the workspace.
WORKSPACE_MOUNT="$(nsenter --target "$DEV_PID" --mount -- sh -c 'df --output=target 2>/dev/null | grep workspace | head -1' 2>/dev/null || echo "/workspace")"
POST_ATTACH="${WORKSPACE_MOUNT}/.okdev/post-attach.sh"
if [ -t 0 ] && nsenter --target "$DEV_PID" --mount -- test -x "$POST_ATTACH" 2>/dev/null; then
  nsenter --target "$DEV_PID" --mount --uts --ipc --pid -- "$POST_ATTACH" 2>&1 || \
    echo "warning: postAttach script failed" >&2
fi
```

Note: the workspace mount path is not known to the sidecar. The simplest approach is to check a well-known path. Since the workspace is mounted at `cfg.Spec.Workspace.MountPath` on both the dev container and sidecar, and the sidecar uses nsenter into the dev container's mount namespace, the path is whatever the user configured. For simplicity, check `/workspace/.okdev/post-attach.sh` as the default, and also set an env var `OKDEV_WORKSPACE` on the sidecar so it can find the right path.

Actually, simpler: set `OKDEV_WORKSPACE` env var on the sidecar container in `PreparePodSpec` (it already knows `workspaceMountPath`). Then the script uses `$OKDEV_WORKSPACE`.

**Step 2: Update PreparePodSpec to always set OKDEV_WORKSPACE env var on sidecar**

In `internal/kube/podspec.go`, add to the sidecar container env:

```go
sidecarContainer.Env = append(sidecarContainer.Env, corev1.EnvVar{
    Name:  "OKDEV_WORKSPACE",
    Value: workspaceMountPath,
})
```

**Step 3: Update nsenter-dev.sh postAttach to use OKDEV_WORKSPACE**

```bash
# Run postAttach script if it exists in the workspace.
if [ -t 0 ] && [ -n "${OKDEV_WORKSPACE:-}" ]; then
  POST_ATTACH="${OKDEV_WORKSPACE}/.okdev/post-attach.sh"
  if nsenter --target "$DEV_PID" --mount -- test -x "$POST_ATTACH" 2>/dev/null; then
    nsenter --target "$DEV_PID" --mount --uts --ipc --pid -- "$POST_ATTACH" 2>&1 || \
      echo "warning: postAttach script failed" >&2
  fi
fi
```

**Step 4: Verify syntax**

Run: `sh -n infra/sidecar/entrypoint.sh`
Expected: No output (valid)

**Step 5: Build Go changes**

Run: `go build ./internal/kube/ && go test ./internal/kube/ -v`
Expected: ALL PASS (update any tests broken by new env var)

**Step 6: Commit**

```bash
git add infra/sidecar/entrypoint.sh internal/kube/podspec.go internal/kube/podspec_test.go
git commit -m "feat(sidecar): add postAttach lifecycle hook and OKDEV_WORKSPACE env var"
```

---

### Summary of changes by file

| File | Change |
|------|--------|
| `internal/config/loader.go` | Add `FolderFile` constant, update discovery order |
| `internal/config/loader_test.go` | Add tests for `.okdev/okdev.yaml` resolution |
| `internal/config/config.go` | Add `LifecycleSpec` struct, add `Lifecycle` field to `DevEnvSpec` |
| `internal/config/config_test.go` | Add tests for lifecycle spec parsing |
| `internal/kube/client.go` | Add `AnnotatePod` and `GetPodAnnotation` methods |
| `internal/kube/podspec.go` | Add `preStop` param, wire lifecycle hook on dev container, add `OKDEV_WORKSPACE` env |
| `internal/kube/podspec_test.go` | Add preStop tests, update existing test signatures |
| `internal/cli/up.go` | Wire postCreate execution + annotation, pass preStop to PreparePodSpec |
| `internal/cli/lifecycle.go` | New file with `resolvePostCreateCommand` and `resolvePreStopCommand` |
| `infra/sidecar/entrypoint.sh` | Add postAttach script execution in nsenter-dev.sh |
