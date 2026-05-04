package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestNewRootCmdRegistersExpectedCommandsAndFlags(t *testing.T) {
	cmd := NewRootCmd()
	if cmd.Use != "okdev" {
		t.Fatalf("unexpected root use %q", cmd.Use)
	}
	if !cmd.SilenceUsage {
		t.Fatal("expected root command to suppress usage for runtime errors")
	}
	if !cmd.SilenceErrors {
		t.Fatal("expected root command to suppress cobra error printing")
	}
	for _, name := range []string{
		"version", "init", "validate", "up", "down", "status", "list", "use",
		"target", "agent", "exec", "logs", "ssh", "ssh-proxy", "ports", "port-forward", "sync", "prune", "migrate", "completion",
	} {
		if _, _, err := cmd.Find([]string{name}); err != nil {
			t.Fatalf("expected subcommand %q: %v", name, err)
		}
	}
	for _, flag := range []string{"config", "session", "owner", "namespace", "context", "output", "verbose"} {
		if got := cmd.PersistentFlags().Lookup(flag); got == nil {
			t.Fatalf("expected persistent flag %q", flag)
		}
	}
}

func TestExecuteDoesNotPrintTelemetryNotice(t *testing.T) {
	t.Setenv(strings.Join([]string{"OKDEV", "POSTHOG", "KEY"}, "_"), "phc_test_key")
	t.Setenv("HOME", t.TempDir())

	originalArgs := os.Args
	os.Args = []string{"okdev", "version"}
	defer func() { os.Args = originalArgs }()

	stdout, stderr, err := captureStandardStreams(func() error {
		return Execute()
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if strings.Contains(stderr, "telemetry") || strings.Contains(stderr, "Notice: okdev collects anonymous usage telemetry") {
		t.Fatalf("expected no telemetry notice, got stderr %q", stderr)
	}
	if !strings.Contains(stdout, "okdev ") {
		t.Fatalf("expected version output, got stdout %q", stdout)
	}
}

func TestExecuteWithContextPropagatesCanceledContext(t *testing.T) {
	root := &cobra.Command{Use: "okdev"}
	root.AddCommand(&cobra.Command{
		Use: "observe",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !errors.Is(cmd.Context().Err(), context.Canceled) {
				return fmt.Errorf("expected canceled command context, got %v", cmd.Context().Err())
			}
			return cmd.Context().Err()
		},
	})
	root.SetArgs([]string{"observe"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := executeWithContext(ctx, root)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context canceled, got %v", err)
	}
}

func captureStandardStreams(fn func() error) (stdout string, stderr string, err error) {
	origStdout := os.Stdout
	origStderr := os.Stderr

	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		return "", "", err
	}
	defer stdoutReader.Close()

	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		stdoutWriter.Close()
		return "", "", err
	}
	defer stderrReader.Close()

	os.Stdout = stdoutWriter
	os.Stderr = stderrWriter
	defer func() {
		os.Stdout = origStdout
		os.Stderr = origStderr
	}()

	runErr := fn()

	stdoutWriter.Close()
	stderrWriter.Close()

	var stdoutBuf bytes.Buffer
	var stderrBuf bytes.Buffer
	if _, err := io.Copy(&stdoutBuf, stdoutReader); err != nil {
		return "", "", err
	}
	if _, err := io.Copy(&stderrBuf, stderrReader); err != nil {
		return "", "", err
	}

	return stdoutBuf.String(), stderrBuf.String(), runErr
}
