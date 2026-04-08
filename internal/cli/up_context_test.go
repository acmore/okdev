package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUpCommandContextUsesDefaultFloor(t *testing.T) {
	ctx, cancel := upCommandContext(30 * time.Second)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected context deadline")
	}
	remaining := time.Until(deadline)
	if remaining < 4*time.Minute || remaining > 5*time.Minute+5*time.Second {
		t.Fatalf("expected default timeout near 5m, got %s", remaining)
	}
}

func TestUpCommandContextExpandsForLargeWaitTimeout(t *testing.T) {
	ctx, cancel := upCommandContext(10 * time.Minute)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected context deadline")
	}
	remaining := time.Until(deadline)
	expected := (2 * 10 * time.Minute) + (2 * time.Minute)
	if remaining < expected-5*time.Second || remaining > expected+5*time.Second {
		t.Fatalf("expected timeout near %s, got %s", expected, remaining)
	}
}

func TestUpCommandContextIsCancellable(t *testing.T) {
	ctx, cancel := upCommandContext(time.Minute)
	cancel()

	select {
	case <-ctx.Done():
	default:
		t.Fatal("expected cancelled context to be done")
	}
	if ctx.Err() != context.Canceled {
		t.Fatalf("expected context canceled, got %v", ctx.Err())
	}
}

func TestUpValidateUsesResolvedOptionsContext(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, ".okdev.yaml")
	if err := os.WriteFile(cfgPath, []byte(""+
		"apiVersion: okdev.io/v1alpha1\n"+
		"kind: DevEnvironment\n"+
		"metadata:\n"+
		"  name: demo\n"+
		"spec:\n"+
		"  namespace: demo\n"+
		"  kubeContext: team-staging\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := &Options{ConfigPath: cfgPath, Session: "sess-a"}
	cmd := newUpCmd(opts)
	var out discardWriter
	cmd.SetOut(out)
	cmd.SetErr(out)

	state, err := upValidate(cmd, opts, upOptions{})
	if err != nil {
		t.Fatalf("upValidate: %v", err)
	}
	defer state.cancel()

	if state.opts == opts {
		t.Fatal("expected up state to use resolved options clone")
	}
	if state.opts.Context != "team-staging" {
		t.Fatalf("expected resolved context, got %q", state.opts.Context)
	}
	if state.command.opts.Context != "team-staging" {
		t.Fatalf("expected command context to use resolved context, got %q", state.command.opts.Context)
	}
	if opts.Context != "" {
		t.Fatalf("expected original opts to remain unchanged, got %q", opts.Context)
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) {
	return len(p), nil
}
