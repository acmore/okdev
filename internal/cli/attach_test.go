package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
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
