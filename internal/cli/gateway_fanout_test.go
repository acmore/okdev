package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/fanout"
	"github.com/acmore/okdev/internal/kube"
)

func gwPods(n int, withIP bool) []kube.PodSummary {
	pods := make([]kube.PodSummary, 0, n)
	for i := 0; i < n; i++ {
		p := kube.PodSummary{Name: "pod-" + string(rune('a'+i)), Phase: "Running"}
		if withIP {
			p.PodIP = "10.0.0." + string(rune('1'+i))
		}
		pods = append(pods, p)
	}
	return pods
}

func TestUseGatewayFanoutDecision(t *testing.T) {
	cases := []struct {
		name     string
		mode     string
		interPod bool
		pods     []kube.PodSummary
		override string
		want     bool
	}{
		{"auto with enough pods", config.ExecFanoutAuto, true, gwPods(4, true), "", true},
		{"auto below threshold", config.ExecFanoutAuto, true, gwPods(3, true), "", false},
		{"auto below threshold with explicit gateway", config.ExecFanoutAuto, true, gwPods(2, true), "pod-a", true},
		{"gateway mode small fleet", config.ExecFanoutGateway, true, gwPods(2, true), "", true},
		{"direct mode always direct", config.ExecFanoutDirect, true, gwPods(10, true), "", false},
		{"no interpod", config.ExecFanoutAuto, false, gwPods(10, true), "", false},
		{"missing pod ip", config.ExecFanoutAuto, true, gwPods(10, false), "", false},
		{"empty mode behaves like auto", "", true, gwPods(4, true), "", true},
		{"unknown mode is direct", "bogus", true, gwPods(10, true), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			route := fanoutRoute{mode: tc.mode, interPod: tc.interPod, user: "root", gatewayOverride: tc.override}
			if got := useGatewayFanout(route, tc.pods); got != tc.want {
				t.Fatalf("useGatewayFanout = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSelectGatewayPodPrefersMasterThenFirstRunning(t *testing.T) {
	pods := []kube.PodSummary{
		{Name: "w-0", Phase: "Running", Labels: map[string]string{"okdev.io/workload-role": "worker"}},
		{Name: "m-0", Phase: "Running", Labels: map[string]string{"okdev.io/workload-role": "master"}},
	}
	gw, err := selectGatewayPod(pods, "")
	if err != nil || gw.Name != "m-0" {
		t.Fatalf("expected master preferred, got %q err=%v", gw.Name, err)
	}

	pods[1].Phase = "Pending"
	gw, err = selectGatewayPod(pods, "")
	if err != nil || gw.Name != "w-0" {
		t.Fatalf("expected first running fallback, got %q err=%v", gw.Name, err)
	}

	if _, err := selectGatewayPod(pods, "absent"); err == nil {
		t.Fatalf("expected error for override not in selection")
	}

	gw, err = selectGatewayPod(pods, "w-0")
	if err != nil || gw.Name != "w-0" {
		t.Fatalf("expected override honored, got %q err=%v", gw.Name, err)
	}
}

// gatewayDriverFake simulates the gateway pod: when the exec command is the
// fanout driver invocation, it decodes the request from stdin and emits the
// configured frames, mimicking a live (or stale) sidecar.
type gatewayDriverFake struct {
	// respond builds the driver stdout from the received request; nil
	// simulates a pre-fanout sidecar (binary missing → error, no hello).
	respond func(req fanout.Request) string
	execErr error
	gotReq  *fanout.Request
}

func (g *gatewayDriverFake) ExecInteractive(_ context.Context, _, pod string, _ bool, command []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(command) == 2 && command[0] == gatewayDriverPath && command[1] == "fanout" {
		if g.respond == nil {
			io.WriteString(stderr, "sh: "+gatewayDriverPath+": not found")
			return errors.New("command terminated with exit code 127")
		}
		var req fanout.Request
		if err := json.NewDecoder(stdin).Decode(&req); err != nil {
			return err
		}
		g.gotReq = &req
		io.WriteString(stdout, g.respond(req))
		return g.execErr
	}
	// Direct-exec fallback path: echo the pod name so tests can tell the
	// direct path ran.
	io.WriteString(stdout, "direct:"+pod)
	return nil
}

func (g *gatewayDriverFake) ExecInteractiveInContainer(ctx context.Context, namespace, pod, _ string, tty bool, command []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return g.ExecInteractive(ctx, namespace, pod, tty, command, stdin, stdout, stderr)
}

func (g *gatewayDriverFake) CopyToPodInContainer(context.Context, string, string, string, string, string) error {
	return nil
}

func framesFor(results ...fanout.Result) func(fanout.Request) string {
	return func(fanout.Request) string {
		var b strings.Builder
		hello, _ := fanout.EncodeHello()
		b.WriteString(hello)
		for _, r := range results {
			f, _ := fanout.EncodeResult(r)
			b.WriteString(f)
		}
		done, _ := fanout.EncodeDone(len(results))
		b.WriteString(done)
		return b.String()
	}
}

func gatewayTestConfig(mode string) *config.DevEnvironment {
	cfg := &config.DevEnvironment{}
	cfg.Spec.Exec.FanoutMode = mode
	interPod := true
	cfg.Spec.SSH.InterPod = &interPod
	cfg.Spec.SSH.User = "root"
	return cfg
}

func TestRunExecJSONRoutedGatewayPath(t *testing.T) {
	pods := gwPods(4, true)
	fake := &gatewayDriverFake{respond: framesFor(
		fanout.Result{Pod: "pod-a", Status: fanout.StatusResponded, Exit: 0, Stdout: []byte("out-a")},
		fanout.Result{Pod: "pod-b", Status: fanout.StatusResponded, Exit: 3, Stderr: []byte("boom")},
		fanout.Result{Pod: "pod-c", Status: fanout.StatusUnreachable, Exit: -1, Error: "dial tcp: refused", Attempts: 3},
		// pod-d intentionally missing.
	)}
	cfg := gatewayTestConfig(config.ExecFanoutAuto)

	var out, errOut strings.Builder
	err := runExecJSONRouted(context.Background(), fake, cfg, "default", pods, "dev", execInvocation{Argv: []string{"echo", "hi there"}}, 30*time.Second, 8, "", false, &out, &errOut)
	if err != nil {
		t.Fatalf("runExecJSONRouted: %v", err)
	}

	if fake.gotReq == nil {
		t.Fatalf("driver was never invoked")
	}
	if fake.gotReq.Command != "'echo' 'hi there'" {
		t.Fatalf("unexpected driver command %q", fake.gotReq.Command)
	}
	if len(fake.gotReq.Targets) != 4 || fake.gotReq.Targets[0].Addr == "" {
		t.Fatalf("unexpected targets: %+v", fake.gotReq.Targets)
	}
	if fake.gotReq.TimeoutSec != 30 {
		t.Fatalf("timeout not forwarded: %d", fake.gotReq.TimeoutSec)
	}

	results := decodeJSONResults(t, out.String())
	if len(results) != 4 {
		t.Fatalf("expected 4 envelopes, got %d: %s", len(results), out.String())
	}
	byPod := map[string]execJSONResult{}
	for _, r := range results {
		byPod[r.Pod] = r
	}
	if r := byPod["pod-a"]; r.Status != fanout.StatusResponded || r.Exit != 0 || r.Stdout != "out-a" {
		t.Fatalf("pod-a envelope wrong: %+v", r)
	}
	if r := byPod["pod-b"]; r.Status != fanout.StatusResponded || r.Exit != 3 || r.Stderr != "boom" || r.Error != "" {
		t.Fatalf("pod-b envelope wrong: %+v", r)
	}
	if r := byPod["pod-c"]; r.Status != fanout.StatusUnreachable || r.Exit != -1 {
		t.Fatalf("pod-c envelope wrong: %+v", r)
	}
	if r := byPod["pod-d"]; r.Status != fanout.StatusMissing || r.Exit != -1 {
		t.Fatalf("pod-d envelope wrong: %+v", r)
	}
}

func TestRunExecJSONRoutedFallsBackWhenDriverMissing(t *testing.T) {
	pods := gwPods(4, true)
	fake := &gatewayDriverFake{respond: nil} // stale sidecar: no hello
	cfg := gatewayTestConfig(config.ExecFanoutAuto)

	var out, errOut strings.Builder
	err := runExecJSONRouted(context.Background(), fake, cfg, "default", pods, "dev", execInvocation{Argv: []string{"true"}}, 0, 8, "", false, &out, &errOut)
	if err != nil {
		t.Fatalf("runExecJSONRouted: %v", err)
	}
	if !strings.Contains(errOut.String(), "falling back to direct exec") {
		t.Fatalf("expected fallback notice, got %q", errOut.String())
	}
	results := decodeJSONResults(t, out.String())
	if len(results) != 4 {
		t.Fatalf("expected 4 direct envelopes, got %d", len(results))
	}
	for _, r := range results {
		if !strings.HasPrefix(r.Stdout, "direct:") || r.Status != fanout.StatusResponded {
			t.Fatalf("expected direct-path result, got %+v", r)
		}
	}
}

func TestRunExecJSONRoutedRequireAll(t *testing.T) {
	pods := gwPods(4, true)
	fake := &gatewayDriverFake{respond: framesFor(
		fanout.Result{Pod: "pod-a", Status: fanout.StatusResponded, Exit: 0},
		fanout.Result{Pod: "pod-b", Status: fanout.StatusResponded, Exit: 1}, // non-zero exit is NOT a violation
		fanout.Result{Pod: "pod-c", Status: fanout.StatusTimeout, Exit: -1},
		// pod-d missing.
	)}
	cfg := gatewayTestConfig(config.ExecFanoutAuto)

	var out, errOut strings.Builder
	err := runExecJSONRouted(context.Background(), fake, cfg, "default", pods, "dev", execInvocation{Argv: []string{"true"}}, 0, 8, "", true, &out, &errOut)
	if err == nil {
		t.Fatalf("expected require-all violation")
	}
	msg := err.Error()
	if !strings.Contains(msg, "pod-c (timeout)") || !strings.Contains(msg, "pod-d (missing)") {
		t.Fatalf("expected offending pods in error, got %q", msg)
	}
	if strings.Contains(msg, "pod-b") {
		t.Fatalf("non-zero exit must not violate require-all: %q", msg)
	}
	// The JSON document must still be emitted before the error.
	if results := decodeJSONResults(t, out.String()); len(results) != 4 {
		t.Fatalf("expected document despite violation, got %s", out.String())
	}
}

func TestCollectDetachJobsViaGateway(t *testing.T) {
	pods := gwPods(4, true)
	meta := `{"jobId":"j1","pod":"pod-a","container":"dev","pid":42,"state":"running","stdoutPath":"/tmp/okdev-exec/j1.log","metaPath":"/tmp/okdev-exec/j1.json","startedAt":"2026-07-03T00:00:00Z","command":"sleep 100"}`
	fake := &gatewayDriverFake{respond: framesFor(
		fanout.Result{Pod: "pod-a", Status: fanout.StatusResponded, Exit: 0, Stdout: []byte("1\t" + meta + "\n")},
		fanout.Result{Pod: "pod-b", Status: fanout.StatusResponded, Exit: 0, Stdout: nil}, // no jobs
		fanout.Result{Pod: "pod-c", Status: fanout.StatusResponded, Exit: 2, Stderr: []byte("sh: boom")},
		fanout.Result{Pod: "pod-d", Status: fanout.StatusUnreachable, Exit: -1, Error: "dial tcp: refused"},
	)}
	route := fanoutRoute{mode: config.ExecFanoutAuto, interPod: true, user: "root"}

	rows, podErrors, ok := collectDetachJobsViaGateway(context.Background(), fake, "default", route, pods, "dev", "", 8)
	if !ok {
		t.Fatalf("expected gateway path to be authoritative")
	}
	if len(rows) != 1 || rows[0].Pod != "pod-a" || rows[0].JobID != "j1" || rows[0].PID != 42 {
		t.Fatalf("unexpected rows: %+v", rows)
	}
	if len(podErrors) != 2 {
		t.Fatalf("expected 2 pod errors, got %+v", podErrors)
	}
	if podErrors[0].Pod != "pod-c" || !strings.Contains(podErrors[0].Error, "exited 2") {
		t.Fatalf("pod-c error wrong: %+v", podErrors[0])
	}
	if podErrors[1].Pod != "pod-d" || !strings.Contains(podErrors[1].Error, "unreachable") {
		t.Fatalf("pod-d error wrong: %+v", podErrors[1])
	}
	if fake.gotReq == nil || !strings.Contains(fake.gotReq.Command, "'sh' '-c'") {
		t.Fatalf("expected listing command via sh -c, got %+v", fake.gotReq)
	}
}

func TestCollectDetachJobsViaGatewayFallsBackToDirect(t *testing.T) {
	pods := gwPods(4, true)
	fake := &gatewayDriverFake{respond: nil} // stale sidecar
	route := fanoutRoute{mode: config.ExecFanoutAuto, interPod: true, user: "root"}

	_, _, ok := collectDetachJobsViaGateway(context.Background(), fake, "default", route, pods, "dev", "", 8)
	if ok {
		t.Fatalf("expected fallback signal when driver is unavailable")
	}

	// Ineligible route (no interpod) must not even attempt the driver.
	fake2 := &gatewayDriverFake{respond: framesFor()}
	_, _, ok = collectDetachJobsViaGateway(context.Background(), fake2, "default", fanoutRoute{mode: config.ExecFanoutAuto}, pods, "dev", "", 8)
	if ok || fake2.gotReq != nil {
		t.Fatalf("ineligible route must skip the gateway entirely")
	}
}

func TestRunExecJSONRoutedDirectModeSkipsGateway(t *testing.T) {
	pods := gwPods(8, true)
	fake := &gatewayDriverFake{respond: framesFor()} // would emit hello if called
	cfg := gatewayTestConfig(config.ExecFanoutDirect)

	var out, errOut strings.Builder
	if err := runExecJSONRouted(context.Background(), fake, cfg, "default", pods, "dev", execInvocation{Argv: []string{"true"}}, 0, 8, "", false, &out, &errOut); err != nil {
		t.Fatalf("runExecJSONRouted: %v", err)
	}
	if fake.gotReq != nil {
		t.Fatalf("gateway driver must not run in direct mode")
	}
}
