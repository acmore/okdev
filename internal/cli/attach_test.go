package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
	"github.com/spf13/cobra"
)

func TestAttachOnlyRejectsLifecycleBeforeClusterContact(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	p := filepath.Join(t.TempDir(), "access.yaml")
	if err := os.WriteFile(p, []byte("apiVersion: okdev.io/v1alpha1\nkind: DevEnvironment\nmetadata: {name: access}\nspec:\n  attachOnly: {pods: [external-0], container: app}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"up"}, {"up", "--dry-run"}, {"up", "--reconcile", "--yes"}, {"down", "--yes"}, {"restart", "--yes"}, {"restart", "--pod", "external-0", "--yes"}, {"sync"}, {"ssh"}, {"status"}, {"status", "--details"}, {"logs"}} {
		root := NewRootCmd()
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		root.SetArgs(append([]string{"--config", p}, args...))
		err := root.Execute()
		if err == nil || !strings.Contains(err.Error(), "attach-only") {
			t.Fatalf("%v: %v", args, err)
		}
	}
}

type attachPodReader struct {
	pods     []kube.PodSummary
	selector string
}

func (r *attachPodReader) ListPods(_ context.Context, _ string, _ bool, selector string) ([]kube.PodSummary, error) {
	r.selector = selector
	return r.pods, nil
}
func (r *attachPodReader) GetPodSummary(_ context.Context, _, name string) (*kube.PodSummary, error) {
	for _, p := range r.pods {
		if p.Name == name {
			return &p, nil
		}
	}
	return nil, nil
}

func TestAttachScopeOwnershipAndNoPinnedTargetReuse(t *testing.T) {
	r := &attachPodReader{pods: []kube.PodSummary{{Name: "a"}, {Name: "b", Labels: map[string]string{"okdev.io/owner": "other"}}}}
	opts := &Options{Owner: "me", attachOnly: &config.AttachOnlySpec{Selector: "app=external", Container: "app"}}
	if _, err := listAttachPods(context.Background(), opts, r, "ns"); err == nil || !strings.Contains(err.Error(), "owned by") {
		t.Fatalf("ownership bypass: %v", err)
	}
	opts.attachOnly = &config.AttachOnlySpec{Pods: []string{"a"}, Container: "app"}
	target, err := resolveAttachTarget(context.Background(), opts, r, "ns")
	if err != nil || target.PodName != "a" || target.Container != "app" {
		t.Fatalf("%+v %v", target, err)
	}
	opts.attachOnly = &config.AttachOnlySpec{Session: "live", Container: "app"}
	r.pods = r.pods[:1]
	if _, err := listAttachPods(context.Background(), opts, r, "ns"); err != nil {
		t.Fatal(err)
	}
	if r.selector != "okdev.io/managed=true,okdev.io/session=live" {
		t.Fatal(r.selector)
	}
}

func attachOnlyConfigFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "access.yaml")
	body := "apiVersion: okdev.io/v1alpha1\nkind: DevEnvironment\nmetadata: {name: access}\nspec:\n  attachOnly: {pods: [external-0], container: app}\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// These two refusals must land before the CLI reaches the cluster, so an
// unreachable API cannot mask them with a connection error instead.
func TestAttachOnlyRejectsExecFlagsBeforeClusterContact(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	path := attachOnlyConfigFile(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"exec", "--gateway", "external-0", "--", "true"}, "--gateway is unavailable in attach-only mode"},
		{[]string{"exec", "--preflight-retry-timeout", "5s", "--", "true"}, "--preflight-retry-timeout is not supported in attach-only mode"},
	} {
		root := NewRootCmd()
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		root.SetArgs(append([]string{"--config", path}, tc.args...))
		err := root.Execute()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%v: got %v, want %q", tc.args, err, tc.want)
		}
	}
}

// These two sit behind session access, so the CLI reaches them only against a
// live cluster; check them where they are decided.
func TestAttachOnlyRejectsSyncGateAndSSHTransport(t *testing.T) {
	cfg := &config.DevEnvironment{}
	cfg.Spec.AttachOnly = &config.AttachOnlySpec{Pods: []string{"external-0"}, Container: "app"}
	cc := &commandContext{opts: &Options{attachOnly: cfg.Spec.AttachOnly}, cfg: cfg, namespace: "ns", sessionName: "access"}
	cmd := &cobra.Command{}
	cmd.SetOut(io.Discard)

	var warnings bytes.Buffer
	cmd.SetErr(&warnings)
	if err := execSyncPreflight(cmd, cc, true, "the command"); err == nil ||
		!strings.Contains(err.Error(), "--require-sync is unavailable in attach-only mode") {
		t.Fatalf("--require-sync: %v", err)
	}
	// Without the gate the command proceeds, and has no channel to warn about.
	if err := execSyncPreflight(cmd, cc, false, "the command"); err != nil || warnings.Len() != 0 {
		t.Fatalf("plain run: err=%v warnings=%q", err, warnings.String())
	}
	if _, err := prepareExecSSH(context.Background(), cc, nil, "app"); err == nil ||
		!strings.Contains(err.Error(), "attach-only uses kubernetes") {
		t.Fatalf("--transport=ssh: %v", err)
	}
}

// No attach-only command registers --workload today, so this guard is only
// reachable at the function that owns it. Keep it honest in case one does.
func TestAttachOnlyRejectsWorkloadSelection(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := attachOnlyConfigFile(t)
	_, _, err := loadConfigAndNamespace(&Options{ConfigPath: path, commandName: "exec", Workload: "other"})
	if err == nil || !strings.Contains(err.Error(), "--workload is unavailable in attach-only mode") {
		t.Fatalf("got %v", err)
	}
	if _, _, err := loadConfigAndNamespace(&Options{ConfigPath: path, commandName: "exec"}); err != nil && strings.Contains(err.Error(), "--workload") {
		t.Fatalf("refused without the flag: %v", err)
	}
}
