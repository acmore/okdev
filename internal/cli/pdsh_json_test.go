package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/acmore/okdev/internal/kube"
)

// jsonCaptureClient writes configurable stdout/stderr per pod and returns a
// configurable error, so the --json capture path can be exercised without a
// cluster.
type jsonCaptureClient struct {
	stdout map[string]string
	stderr map[string]string
	errs   map[string]error
}

func (c *jsonCaptureClient) ExecInteractive(_ context.Context, _, pod string, _ bool, _ []string, _ io.Reader, stdout, stderr io.Writer) error {
	if s := c.stdout[pod]; s != "" {
		fmt.Fprint(stdout, s)
	}
	if s := c.stderr[pod]; s != "" {
		fmt.Fprint(stderr, s)
	}
	return c.errs[pod]
}

func (c *jsonCaptureClient) ExecInteractiveInContainer(ctx context.Context, namespace, pod, _ string, tty bool, command []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return c.ExecInteractive(ctx, namespace, pod, tty, command, stdin, stdout, stderr)
}

func (c *jsonCaptureClient) CopyToPodInContainer(context.Context, string, string, string, string, string) error {
	return nil
}

func decodeJSONResults(t *testing.T, raw string) []execJSONResult {
	t.Helper()
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "[") {
		var arr []execJSONResult
		if err := json.Unmarshal([]byte(trimmed), &arr); err != nil {
			t.Fatalf("unmarshal array: %v\nraw: %s", err, raw)
		}
		return arr
	}
	var obj execJSONResult
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		t.Fatalf("unmarshal object: %v\nraw: %s", err, raw)
	}
	return []execJSONResult{obj}
}

func TestRunMultiExecJSONSinglePodEmitsLengthOneArray(t *testing.T) {
	client := &jsonCaptureClient{
		stdout: map[string]string{"pod-a": "37\n"},
	}
	pods := []kube.PodSummary{{Name: "pod-a"}}
	var out strings.Builder
	if err := runMultiExecJSON(context.Background(), client, "default", pods, "", execInvocation{Argv: []string{"echo", "37"}}, 0, 4, &out); err != nil {
		t.Fatalf("runMultiExecJSON: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(out.String()), "[") {
		t.Fatalf("expected a length-1 array for one pod, got object: %s", out.String())
	}
	got := decodeJSONResults(t, out.String())
	if len(got) != 1 || got[0].Pod != "pod-a" || got[0].Exit != 0 || got[0].Stdout != "37\n" || got[0].Stderr != "" || got[0].Error != "" {
		t.Fatalf("unexpected envelope: %+v", got[0])
	}
}

func TestRunMultiExecJSONRemoteNonZeroExitIsData(t *testing.T) {
	// pgrep returning 1 ("no match") must surface as exit:1 data, not as an
	// okdev failure, and must not populate the error field.
	client := &jsonCaptureClient{
		stdout: map[string]string{"pod-a": "0\n"},
		errs:   map[string]error{"pod-a": errors.New("command terminated with exit code 1")},
	}
	pods := []kube.PodSummary{{Name: "pod-a"}}
	var out strings.Builder
	if err := runMultiExecJSON(context.Background(), client, "default", pods, "", execInvocation{Argv: []string{"pgrep", "-c", "x"}}, 0, 4, &out); err != nil {
		t.Fatalf("runMultiExecJSON must not fail on remote non-zero exit, got: %v", err)
	}
	got := decodeJSONResults(t, out.String())[0]
	if got.Exit != 1 {
		t.Fatalf("expected exit 1 as data, got %d", got.Exit)
	}
	if got.Stdout != "0\n" {
		t.Fatalf("expected stdout '0\\n', got %q", got.Stdout)
	}
	if got.Error != "" {
		t.Fatalf("remote non-zero exit must not set error field, got %q", got.Error)
	}
}

func TestRunMultiExecJSONInfraErrorPopulatesErrorField(t *testing.T) {
	client := &jsonCaptureClient{
		errs: map[string]error{"pod-a": errors.New("connection refused")},
	}
	pods := []kube.PodSummary{{Name: "pod-a"}}
	var out strings.Builder
	if err := runMultiExecJSON(context.Background(), client, "default", pods, "", execInvocation{Argv: []string{"true"}}, 0, 4, &out); err != nil {
		t.Fatalf("runMultiExecJSON: %v", err)
	}
	got := decodeJSONResults(t, out.String())[0]
	if got.Exit != -1 {
		t.Fatalf("expected exit -1 for non-remote failure, got %d", got.Exit)
	}
	if !strings.Contains(got.Error, "connection refused") {
		t.Fatalf("expected error field to carry the failure reason, got %q", got.Error)
	}
}

func TestRunMultiExecJSONMultiPodEmitsOrderedArray(t *testing.T) {
	client := &jsonCaptureClient{
		stdout: map[string]string{"pod-a": "a-out\n", "pod-b": "b-out\n"},
		stderr: map[string]string{"pod-b": "b-err\n"},
		errs:   map[string]error{"pod-b": errors.New("command terminated with exit code 2")},
	}
	pods := []kube.PodSummary{{Name: "pod-a"}, {Name: "pod-b"}}
	var out strings.Builder
	if err := runMultiExecJSON(context.Background(), client, "default", pods, "", execInvocation{Argv: []string{"echo", "x"}}, 0, 4, &out); err != nil {
		t.Fatalf("runMultiExecJSON: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(out.String()), "[") {
		t.Fatalf("expected array for multiple pods, got: %s", out.String())
	}
	got := decodeJSONResults(t, out.String())
	if len(got) != 2 {
		t.Fatalf("expected 2 envelopes, got %d", len(got))
	}
	if got[0].Pod != "pod-a" || got[1].Pod != "pod-b" {
		t.Fatalf("expected selection order preserved, got %q then %q", got[0].Pod, got[1].Pod)
	}
	if got[0].Exit != 0 || got[0].Stdout != "a-out\n" {
		t.Fatalf("unexpected pod-a envelope: %+v", got[0])
	}
	if got[1].Exit != 2 || got[1].Stdout != "b-out\n" || got[1].Stderr != "b-err\n" {
		t.Fatalf("unexpected pod-b envelope: %+v", got[1])
	}
}

func TestValidateExecJSONFlags(t *testing.T) {
	cases := []struct {
		name       string
		json       bool
		hasCommand bool
		detach     bool
		groups     []string
		parallel   bool
		sequential bool
		logDir     string
		wantErr    string
	}{
		{name: "disabled is always ok", json: false},
		{name: "requires command", json: true, hasCommand: false, wantErr: "requires a command"},
		{name: "ok with command", json: true, hasCommand: true},
		{name: "rejects detach", json: true, hasCommand: true, detach: true, wantErr: "--detach"},
		{name: "rejects group", json: true, hasCommand: true, groups: []string{"a,b"}, wantErr: "--group"},
		{name: "rejects parallel", json: true, hasCommand: true, parallel: true, wantErr: "--parallel"},
		{name: "rejects sequential", json: true, hasCommand: true, sequential: true, wantErr: "--sequential"},
		{name: "rejects log-dir", json: true, hasCommand: true, logDir: "/tmp/logs", wantErr: "--log-dir"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateExecJSONFlags(tc.json, tc.hasCommand, tc.detach, tc.groups, tc.parallel, tc.sequential, tc.logDir)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}
