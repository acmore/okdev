package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/connect"
	"github.com/acmore/okdev/internal/fanout"
	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/shellutil"
)

const (
	// gatewayFanoutMinPods is the auto-mode threshold: below this, direct
	// per-pod exec is cheap enough that the gateway hop adds latency for no
	// meaningful apiserver relief.
	gatewayFanoutMinPods = 4
	// gatewayDriverPath is where the sidecar entrypoint stages the okdev-sshd
	// binary inside the shared /var/okdev volume; the same binary carries the
	// fanout subcommand.
	gatewayDriverPath = "/var/okdev/okdev-sshd"
	// gatewayInterPodKeyPath matches the identity file written to every pod
	// by the interpod SSH setup. The ~/ prefix is expanded by the driver in
	// the dev container.
	gatewayInterPodKeyPath = "~/.ssh/okdev_interpod_ed25519"
	gatewaySSHPort         = 2222
	gatewayFanoutRetries   = 2
)

// fanoutRoute carries everything the routing decision needs, derived once
// from the command context so the exec and jobs paths share it.
type fanoutRoute struct {
	mode            string
	interPod        bool
	user            string
	gatewayOverride string
}

func fanoutRouteFromConfig(cfg *config.DevEnvironment, gatewayOverride string) fanoutRoute {
	return fanoutRoute{
		mode:            cfg.Spec.Exec.FanoutMode,
		interPod:        cfg.Spec.SSH.InterPodEnabled(),
		user:            cfg.Spec.SSH.User,
		gatewayOverride: gatewayOverride,
	}
}

// useGatewayFanout decides whether this invocation should attempt the
// gateway path. Gateway fanout requires the interpod SSH channel and pod IPs
// for every target; auto mode additionally requires enough pods for the
// apiserver relief to matter.
func useGatewayFanout(route fanoutRoute, pods []kube.PodSummary) bool {
	switch route.mode {
	case config.ExecFanoutDirect:
		return false
	case config.ExecFanoutGateway:
	case config.ExecFanoutAuto, "":
		if len(pods) < gatewayFanoutMinPods && route.gatewayOverride == "" {
			return false
		}
	default:
		return false
	}
	if !route.interPod {
		return false
	}
	for _, pod := range pods {
		if strings.TrimSpace(pod.PodIP) == "" {
			return false
		}
	}
	return true
}

// selectGatewayPod prefers the explicit override, then the workload master,
// then the first running pod in selection order.
func selectGatewayPod(pods []kube.PodSummary, override string) (kube.PodSummary, error) {
	if strings.TrimSpace(override) != "" {
		for _, pod := range pods {
			if pod.Name == override {
				return pod, nil
			}
		}
		return kube.PodSummary{}, fmt.Errorf("gateway pod %q is not among the selected pods", override)
	}
	var firstRunning *kube.PodSummary
	for i, pod := range pods {
		if pod.Phase != "Running" {
			continue
		}
		if strings.TrimSpace(pod.Labels["okdev.io/workload-role"]) == "master" {
			return pod, nil
		}
		if firstRunning == nil {
			firstRunning = &pods[i]
		}
	}
	if firstRunning != nil {
		return *firstRunning, nil
	}
	return kube.PodSummary{}, fmt.Errorf("no running pod available as fanout gateway")
}

// buildFanoutRequest translates an exec invocation into the driver request.
func buildFanoutRequest(route fanoutRoute, pods []kube.PodSummary, invocation execInvocation, timeout time.Duration, fanoutN int) (fanout.Request, error) {
	targets := make([]fanout.Target, 0, len(pods))
	for _, pod := range pods {
		targets = append(targets, fanout.Target{Pod: pod.Name, Addr: pod.PodIP})
	}
	req := fanout.Request{
		Version: fanout.ProtocolVersion,
		User:    route.user,
		KeyPath: gatewayInterPodKeyPath,
		Port:    gatewaySSHPort,
		Targets: targets,
		Fanout:  fanoutN,
		Retries: gatewayFanoutRetries,
	}
	if timeout > 0 {
		req.TimeoutSec = int(timeout.Seconds())
		if req.TimeoutSec == 0 {
			req.TimeoutSec = 1
		}
	}
	if invocation.ScriptLocalPath != "" {
		content, err := os.ReadFile(invocation.ScriptLocalPath)
		if err != nil {
			return fanout.Request{}, fmt.Errorf("read script: %w", err)
		}
		req.Script = content
		req.ScriptArgs = invocation.Argv
	} else {
		req.Command = shellJoinArgv(invocation.Argv)
	}
	return req, nil
}

func shellJoinArgv(argv []string) string {
	parts := make([]string, 0, len(argv))
	for _, a := range argv {
		parts = append(parts, shellutil.Quote(a))
	}
	return strings.Join(parts, " ")
}

// gatewayNoRetryPolicy runs the driver exec exactly once. The driver is NOT
// idempotent — rerunning it would re-execute the user's command on every
// pod — so transport retries must never apply here. Reliability comes from
// the driver's own in-cluster dial retries instead.
var gatewayNoRetryPolicy = connect.RetryPolicy{MaxAttempts: 1}

// runGatewayFanout executes the request's command on all pods via the
// gateway driver. The boolean result reports whether the gateway path was
// authoritative: false with a nil error means the driver is unavailable
// (missing binary, pre-fanout sidecar) and the caller should fall back to
// direct exec — which is only safe because no hello frame means no command
// ran anywhere.
func runGatewayFanout(ctx context.Context, client connect.ExecClient, namespace string, gateway kube.PodSummary, container string, req fanout.Request) ([]execJSONResult, bool, error) {
	payload, err := encodeFanoutRequest(req)
	if err != nil {
		return nil, false, err
	}
	var stdout, stderr bytes.Buffer
	execErr := connect.RunOnContainerWithRetry(ctx, client, namespace, gateway.Name, container,
		[]string{gatewayDriverPath, "fanout"}, false, bytes.NewReader(payload), &stdout, &stderr, gatewayNoRetryPolicy)

	stream, parseErr := fanout.ParseStream(bytes.NewReader(stdout.Bytes()))
	if parseErr != nil {
		return nil, false, fmt.Errorf("parse gateway fanout output: %w", parseErr)
	}
	if !stream.HelloSeen {
		// Driver never started: safe to fall back, nothing has run.
		return nil, false, nil
	}
	if execErr != nil && len(stream.Results) == 0 && !stream.DoneSeen {
		// Driver started but died before doing any work (e.g. invalid
		// request, unreadable key). Commands may not have run, but we
		// cannot prove it — surface the error instead of silently
		// re-executing via the direct path.
		return nil, true, fmt.Errorf("gateway fanout driver failed on %s: %v (stderr: %s)", gateway.Name, execErr, collapseWhitespace(stderr.String()))
	}

	byPod := make(map[string]fanout.Result, len(stream.Results))
	for _, r := range stream.Results {
		byPod[r.Pod] = r
	}
	results := make([]execJSONResult, 0, len(req.Targets))
	for _, target := range req.Targets {
		r, ok := byPod[target.Pod]
		if !ok {
			results = append(results, execJSONResult{
				Pod:    target.Pod,
				Exit:   -1,
				Status: fanout.StatusMissing,
				Error:  "no result frame received from gateway",
			})
			continue
		}
		res := execJSONResult{
			Pod:    r.Pod,
			Exit:   r.Exit,
			Stdout: string(r.Stdout),
			Stderr: string(r.Stderr),
			Status: r.Status,
		}
		if r.Error != "" {
			res.Error = collapseWhitespace(r.Error)
		}
		results = append(results, res)
	}
	return results, true, nil
}

func encodeFanoutRequest(req fanout.Request) ([]byte, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(req)
}

// requireAllError returns a non-nil error listing pods whose command outcome
// is unknown (anything but a responded status). A responded pod with a
// non-zero exit code is NOT a violation: the command ran and its exit code
// is data.
func requireAllError(results []execJSONResult) error {
	var missing []string
	for _, r := range results {
		if r.Status != fanout.StatusResponded {
			missing = append(missing, fmt.Sprintf("%s (%s)", r.Pod, r.Status))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("%d of %d pods did not respond: %s", len(missing), len(results), strings.Join(missing, ", "))
}
