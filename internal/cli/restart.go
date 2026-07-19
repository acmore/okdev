package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/workload"
	"github.com/spf13/cobra"
)

func newRestartCmd(opts *Options) *cobra.Command {
	var yes bool
	var waitTimeout time.Duration
	var podNames []string
	var waitHooks bool

	cmd := &cobra.Command{
		Use:   "restart [session]",
		Short: "Recreate the session workload with the same config",
		Long: `Delete the session workload, wait for it to terminate, and bring it back
up from the current config in one command — the recovery path when a
container died and the restart policy will not bring it back.

With --pod, only the named pods are deleted and the workload controller
recreates them in place; the other pods keep running, their caches and
detached jobs untouched. Lifecycle hooks re-run only on the fresh pods
(they are tracked per pod). For PyTorchJob workloads this requires the
replica's restartPolicy to be OnFailure or ExitCode: with Never, the
training-operator marks the whole job Failed instead of recreating the
deleted pod, so restart --pod refuses and explains the fix.

PVCs and other resources referenced by spec.volumes are untouched.
Lifecycle hooks (postCreate/postSync) re-run automatically on the fresh
pods, and sync re-bootstraps against the new workload.`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: sessionCompletionFunc(opts),
		Example: `  # Restart the current session (prompts for confirmation)
  okdev restart

  # Restart without confirmation
  okdev restart --yes

  # Recreate a single dead worker, leaving the rest of the session running
  okdev restart --pod worker-1

  # Restart a specific session
  okdev restart my-feature -y`,
		RunE: func(cmd *cobra.Command, args []string) error {
			applySessionArg(opts, args)
			if len(podNames) > 0 {
				return runRestartPods(cmd, opts, podNames, yes, waitTimeout, waitHooks)
			}
			ui := newUpUI(cmd.OutOrStdout(), cmd.ErrOrStderr())
			ui.section("Validate")
			cc, err := resolveCommandContext(opts, resolveSessionName)
			if err != nil {
				return err
			}
			ui.stepDone("config", cc.cfgPath)
			ui.stepDone("session", cc.sessionName)
			ui.stepDone("namespace", cc.namespace)
			runtime, err := sessionRuntimeForExisting(cmd.Context(), cc.cfg, cc.cfgPath, cc.namespace, cc.sessionName, cc.kube)
			if err != nil {
				return err
			}
			if err := ensureSessionOwnership(cc.opts, cc.kube, cc.namespace, cc.sessionName); err != nil {
				return err
			}
			ui.stepDone("ownership", "ok")

			delCtx, cancel := downCommandContext(true, waitTimeout)
			defer cancel()
			exists, err := shouldReuseExistingWorkload(delCtx, cc.kube, cc.namespace, runtime)
			if err != nil {
				return err
			}

			if exists {
				if !yes {
					ok, err := confirmRestart(os.Stdin, cmd.ErrOrStderr(), cc.sessionName, cc.namespace, runtime.Kind(), runtime.WorkloadName())
					if err != nil {
						return err
					}
					if !ok {
						fmt.Fprintln(cmd.OutOrStdout(), "Aborted.")
						return nil
					}
				}
				ui.section("Delete")
				if err := runtime.Delete(delCtx, cc.kube, cc.namespace, true); err != nil {
					return fmt.Errorf("delete workload: %w", err)
				}
				ui.stepRun("workload", "waiting for deletion")
				if _, err := waitForDownDeletion(delCtx, cc.kube, cc.namespace, cc.sessionName, runtime, waitTimeout); err != nil {
					return err
				}
				ui.stepDone("workload", fmt.Sprintf("%s/%s deleted", runtime.Kind(), runtime.WorkloadName()))
			} else {
				ui.stepDone("workload", "already absent")
			}

			// Reset local per-session state (sync daemons, pins) so the up
			// below bootstraps cleanly against the fresh workload.
			payload := downOutput{Cleanup: map[string]any{}}
			if err := downCleanupLocal(ui, &payload, cc.sessionName); err != nil {
				return err
			}

			return runUp(cmd, opts, upOptions{waitTimeout: waitTimeout, waitHooks: waitHooks})
		},
	}

	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip the confirmation prompt")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", upDefaultWaitTimeout, "Timeout for workload deletion and pod readiness")
	cmd.Flags().StringSliceVar(&podNames, "pod", nil, "Recreate only these pods (short names ok); other pods keep running")
	cmd.Flags().BoolVar(&waitHooks, "wait-hooks", false, "Keep converging postSync until every session pod (including late-created ones) has completed it")
	return cmd
}

// runRestartPods recreates individual pods of a controller-backed workload:
// delete the pod, let the controller bring it back, then re-run the
// idempotent per-pod session setup (hooks are annotation-tracked per pod, so
// they replay only on the fresh pods).
func runRestartPods(cmd *cobra.Command, opts *Options, podNames []string, yes bool, waitTimeout time.Duration, waitHooks bool) error {
	ui := newUpUI(cmd.OutOrStdout(), cmd.ErrOrStderr())
	ui.section("Validate")
	cc, err := resolveCommandContext(opts, resolveSessionName)
	if err != nil {
		return err
	}
	ui.stepDone("config", cc.cfgPath)
	ui.stepDone("session", cc.sessionName)
	ui.stepDone("namespace", cc.namespace)
	if err := ensureSessionOwnership(cc.opts, cc.kube, cc.namespace, cc.sessionName); err != nil {
		return err
	}
	ui.stepDone("ownership", "ok")

	if normalizeWorkloadType(cc.cfg.Spec.Workload.Type) == workload.TypePod {
		return fmt.Errorf("--pod applies to multi-pod controller workloads; a pod workload has exactly one pod — run `okdev restart` instead")
	}

	ctx, cancel := upCommandContext(cmd.Context(), waitTimeout)
	defer cancel()
	sessionPods, err := cc.kube.ListPods(ctx, cc.namespace, false, selectorForSessionRun(cc.sessionName))
	if err != nil {
		return fmt.Errorf("list session pods: %w", err)
	}
	targets, err := resolvePodAliases(sessionPods, podNames)
	if err != nil {
		return err
	}
	if err := ensureControllerRecreatesDeletedPods(ctx, cc.kube, cc.namespace, cc.sessionName, targets); err != nil {
		return err
	}

	targetNames := make([]string, len(targets))
	for i, pod := range targets {
		targetNames[i] = pod.Name
	}
	if !yes {
		ok, err := confirmRestartPods(os.Stdin, cmd.ErrOrStderr(), cc.sessionName, cc.namespace, targetNames)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(cmd.OutOrStdout(), "Aborted.")
			return nil
		}
	}

	ui.section("Recreate")
	for _, pod := range targets {
		if err := cc.kube.Delete(ctx, cc.namespace, "pod", pod.Name, false); err != nil {
			return fmt.Errorf("delete pod %s: %w", pod.Name, err)
		}
	}
	ui.stepDone("delete", strings.Join(targetNames, ", "))
	ui.stepRun("recreate", "waiting for the controller to replace the pod(s)")
	if err := waitForPodReplacements(ctx, cc.kube, cc.namespace, cc.sessionName, sessionPods, targets, waitTimeout, func(detail string) {
		ui.stepRun("recreate", detail)
	}); err != nil {
		return err
	}
	ui.stepDone("recreate", strings.Join(targetNames, ", "))

	// Re-run the idempotent up path: readiness wait, SSH/sync reconciliation
	// (sync state resets only if the sync hub itself was recreated), mesh,
	// and per-pod-gated postSync/postCreate on the fresh pods.
	return runUp(cmd, opts, upOptions{waitTimeout: waitTimeout, waitHooks: waitHooks})
}

type restartPodPolicyClient interface {
	GetResourceNestedString(ctx context.Context, namespace, apiVersion, kind, name string, fields ...string) (string, bool, error)
}

// ensureControllerRecreatesDeletedPods refuses single-pod recreation when the
// controller would not bring the pod back. Verified against the kubeflow
// training-operator: deleting a member pod under restartPolicy Never marks
// the whole PyTorchJob Failed without recreating the pod, while OnFailure
// (and ExitCode) recreate it in place under the same name.
func ensureControllerRecreatesDeletedPods(ctx context.Context, k restartPodPolicyClient, namespace, sessionName string, targets []kube.PodSummary) error {
	for _, pod := range targets {
		if !strings.EqualFold(strings.TrimSpace(pod.Labels["okdev.io/workload-type"]), workload.TypePyTorchJob) {
			continue
		}
		workloadName := strings.TrimSpace(pod.Labels["okdev.io/workload-name"])
		role := strings.TrimSpace(pod.Labels["okdev.io/workload-role"])
		if workloadName == "" || role == "" {
			continue
		}
		policy, found, err := k.GetResourceNestedString(ctx, namespace, "kubeflow.org/v1", "PyTorchJob", workloadName, "spec", "pytorchReplicaSpecs", role, "restartPolicy")
		if err != nil {
			return fmt.Errorf("read restartPolicy for %s replica %s: %w", workloadName, role, err)
		}
		if !found || policy == "Never" {
			return fmt.Errorf("pod %s belongs to PyTorchJob replica %q with restartPolicy %q: the training-operator would mark the whole job Failed instead of recreating the deleted pod.\nSet restartPolicy: OnFailure for that replica in the workload manifest, run `okdev up --reconcile`, then retry — or use a full `okdev restart`", pod.Name, role, policyOrNever(policy, found))
		}
	}
	return nil
}

func policyOrNever(policy string, found bool) string {
	if !found {
		return "Never (default)"
	}
	return policy
}

type podReplacementLister interface {
	ListPods(ctx context.Context, namespace string, allNamespaces bool, labelSelector string) ([]kube.PodSummary, error)
}

// waitForPodReplacements waits until every deleted pod has a running
// successor. Controllers with stable pod names (PyTorchJob) are matched by
// name + changed UID; controllers that generate fresh names (Deployments)
// are covered by counting running pods that did not exist before the delete.
func waitForPodReplacements(ctx context.Context, k podReplacementLister, namespace, sessionName string, preDelete, deleted []kube.PodSummary, timeout time.Duration, onProgress func(string)) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	selector := selectorForSessionRun(sessionName)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		pods, err := k.ListPods(waitCtx, namespace, false, selector)
		if err == nil {
			replaced, pending := countPodReplacements(pods, preDelete, deleted)
			if replaced >= len(deleted) {
				return nil
			}
			if onProgress != nil {
				onProgress(fmt.Sprintf("%d/%d replaced, waiting for %s", replaced, len(deleted), strings.Join(pending, ", ")))
			}
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("pod replacement did not appear within %s (is the workload controller healthy?): %w", timeout, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func countPodReplacements(pods, preDelete, deleted []kube.PodSummary) (int, []string) {
	preUIDs := make(map[string]struct{}, len(preDelete))
	for _, pod := range preDelete {
		preUIDs[pod.UID] = struct{}{}
	}
	deletedNames := make(map[string]string, len(deleted))
	for _, pod := range deleted {
		deletedNames[pod.Name] = pod.UID
	}

	running := func(pod kube.PodSummary) bool {
		return !pod.Deleting && strings.EqualFold(pod.Phase, "Running")
	}

	replaced := 0
	freshNew := 0
	reappeared := make(map[string]struct{}, len(deleted))
	for _, pod := range pods {
		if !running(pod) {
			continue
		}
		if oldUID, ok := deletedNames[pod.Name]; ok {
			if pod.UID != oldUID {
				replaced++
				reappeared[pod.Name] = struct{}{}
			}
			continue
		}
		if _, existed := preUIDs[pod.UID]; !existed {
			freshNew++
		}
	}
	var pending []string
	for name := range deletedNames {
		if _, ok := reappeared[name]; !ok {
			pending = append(pending, name)
		}
	}
	sort.Strings(pending)
	if freshNew > len(pending) {
		freshNew = len(pending)
	}
	return replaced + freshNew, pending
}

func confirmRestartPods(in io.Reader, out io.Writer, sessionName, namespace string, podNames []string) (bool, error) {
	if !isTerminalReader(in) {
		return false, fmt.Errorf("refusing to restart pods without --yes in non-interactive mode")
	}
	fmt.Fprintf(out, "Recreate pod(s) %s of session %q in namespace %q? The controller replaces them; lifecycle hooks re-run on the fresh pod(s). [y/N]: ", strings.Join(podNames, ", "), sessionName, namespace)
	reader := bufio.NewReader(in)
	line, err := reader.ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	answer := strings.TrimSpace(strings.ToLower(line))
	return answer == "y" || answer == "yes", nil
}

func confirmRestart(in io.Reader, out io.Writer, sessionName, namespace, kind, workloadName string) (bool, error) {
	if !isTerminalReader(in) {
		return false, fmt.Errorf("refusing to restart without --yes in non-interactive mode")
	}
	fmt.Fprintf(out, "Restart session %q in namespace %q? This deletes and recreates %s/%s. [y/N]: ", sessionName, namespace, kind, workloadName)
	reader := bufio.NewReader(in)
	line, err := reader.ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	answer := strings.TrimSpace(strings.ToLower(line))
	return answer == "y" || answer == "yes", nil
}
