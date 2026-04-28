package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/acmore/okdev/internal/kube"
	"github.com/spf13/cobra"
)

func parseCpArgs(src, dst string) (localPath, remotePath string, upload bool, err error) {
	srcRemote := strings.HasPrefix(src, ":")
	dstRemote := strings.HasPrefix(dst, ":")
	if srcRemote == dstRemote {
		return "", "", false, fmt.Errorf("exactly one of src or dst must start with ':' to indicate the remote path")
	}
	if srcRemote {
		remotePath = strings.TrimPrefix(src, ":")
		localPath = dst
		upload = false
	} else {
		remotePath = strings.TrimPrefix(dst, ":")
		localPath = src
		upload = true
	}
	if remotePath == "" {
		return "", "", false, fmt.Errorf("remote path cannot be empty")
	}
	return localPath, remotePath, upload, nil
}

func newCpCmd(opts *Options) *cobra.Command {
	var allPods bool
	var podNames []string
	var role string
	var labels []string
	var exclude []string
	var container string
	var fanout int
	var readyOnly bool

	cmd := &cobra.Command{
		Use:   "cp [session] <src> <dst>",
		Short: "Copy files to/from session pod(s)",
		Long: "Copy files or directories between local machine and session pods.\n" +
			"Prefix the remote path with ':' (e.g., :/workspace/data).\n" +
			"Multi-pod mode fans out uploads or downloads to per-pod subdirectories.",
		Example: `  # Upload a file to the target pod
  okdev cp ./model.pt :/workspace/model.pt

  # Upload a directory to all pods
  okdev cp --all ./config :/etc/app/config

  # Download from all workers into per-pod subdirectories
  okdev cp --role worker :/workspace/output ./output`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			// The last two args are src and dst; anything before is the session.
			if len(args) > 3 {
				return fmt.Errorf("expected at most 3 positional arguments: [session] <src> <dst>")
			}
			var src, dst string
			if len(args) == 3 {
				opts.Session = args[0]
				src, dst = args[1], args[2]
			} else {
				src, dst = args[0], args[1]
			}

			localPath, remotePath, upload, err := parseCpArgs(src, dst)
			if err != nil {
				return err
			}

			// Validate local source exists for upload before connecting.
			if upload {
				if _, err := os.Stat(localPath); err != nil {
					return fmt.Errorf("local source: %w", err)
				}
			}

			cc, err := resolveCommandContext(opts, resolveSessionName)
			if err != nil {
				return err
			}
			if err := ensureExistingSessionOwnership(cc.opts, cc.kube, cc.namespace, cc.sessionName); err != nil {
				return err
			}

			multiPod := allPods || len(podNames) > 0 || role != "" || len(labels) > 0
			if err := validateCpFlags(allPods, podNames, role, labels, exclude); err != nil {
				return err
			}

			if multiPod {
				return runMultiPodCp(cmd, cc, localPath, remotePath, upload, allPods, podNames, role, labels, exclude, container, fanout, readyOnly)
			}

			// Single-pod mode.
			target, err := resolveTargetRef(cmd.Context(), cc.opts, cc.cfg, cc.namespace, cc.sessionName, cc.kube)
			if err != nil {
				return err
			}
			targetContainer := container
			if targetContainer == "" {
				targetContainer = target.Container
			}
			out := cmd.OutOrStdout()
			prog := newSinglePodProgress(out, singlePodCpMessage(localPath, remotePath, upload))
			prog.start()
			err = runSinglePodCpWithProgress(cmd.Context(), cc.kube, cc.namespace, target.PodName, targetContainer, localPath, remotePath, upload, out, prog)
			prog.stop()
			if err != nil {
				return err
			}
			fmt.Fprintln(out, singlePodCpDoneLine(localPath, remotePath, upload))
			return nil
		},
	}

	cmd.Flags().BoolVar(&allPods, "all", false, "Target all session pods")
	cmd.Flags().StringSliceVar(&podNames, "pod", nil, "Target specific pods by name (repeatable/comma-separated)")
	cmd.Flags().StringVar(&role, "role", "", "Target pods by workload role")
	cmd.Flags().StringSliceVar(&labels, "label", nil, "Target pods by label key=value (repeatable)")
	cmd.Flags().StringSliceVar(&exclude, "exclude", nil, "Exclude specific pods (repeatable/comma-separated)")
	cmd.Flags().StringVar(&container, "container", "", "Override target container")
	cmd.Flags().IntVar(&fanout, "fanout", pdshDefaultFanout, "Maximum concurrent pod transfers")
	cmd.Flags().BoolVar(&readyOnly, "ready-only", false, "Copy only to/from pods that are already running (skip readiness check)")
	return cmd
}

func validateCpFlags(allPods bool, podNames []string, role string, labels []string, exclude []string) error {
	if len(exclude) > 0 && len(podNames) > 0 {
		return fmt.Errorf("--exclude cannot be used with --pod")
	}
	if len(exclude) > 0 && !allPods && role == "" && len(labels) == 0 {
		return fmt.Errorf("--exclude requires --all, --role, or --label")
	}
	exclusiveCount := 0
	if allPods {
		exclusiveCount++
	}
	if len(podNames) > 0 {
		exclusiveCount++
	}
	if role != "" {
		exclusiveCount++
	}
	if len(labels) > 0 {
		exclusiveCount++
	}
	if exclusiveCount > 1 {
		return fmt.Errorf("--all, --pod, --role, and --label are mutually exclusive")
	}
	return nil
}

func runSinglePodCp(ctx context.Context, client *kube.Client, namespace, pod, container, localPath, remotePath string, upload bool, out io.Writer) error {
	return runSinglePodCpWithProgress(ctx, client, namespace, pod, container, localPath, remotePath, upload, out, nil)
}

// runSinglePodCpWithProgress performs a single-pod copy and, if prog is
// non-nil, threads it through to the underlying kube methods so byte and
// file counters are updated as data flows.
func runSinglePodCpWithProgress(ctx context.Context, client *kube.Client, namespace, pod, container, localPath, remotePath string, upload bool, out io.Writer, prog *cpProgress) error {
	kp := kube.CopyProgress{}
	if prog != nil {
		kp.OnBytes = prog.addBytes
		kp.OnFile = prog.addFile
	}
	if upload {
		info, err := os.Stat(localPath)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return client.CopyDirToPodWithProgress(ctx, namespace, pod, container, localPath, remotePath, kp)
		}
		return client.CopyToPodInContainerWithProgress(ctx, namespace, localPath, pod, container, remotePath, kp)
	}

	isDir, err := client.IsRemoteDir(ctx, namespace, pod, container, remotePath)
	if err != nil {
		return err
	}
	if isDir {
		if err := os.MkdirAll(localPath, 0o755); err != nil {
			return err
		}
		return client.CopyDirFromPodWithProgress(ctx, namespace, pod, container, remotePath, localPath, kp)
	}
	targetPath, err := resolveDownloadTargetPath(localPath, "", remotePath, false, 1)
	if err != nil {
		return err
	}
	return client.CopyFromPodInContainerWithProgress(ctx, namespace, pod, container, remotePath, targetPath, kp)
}

func multiPodDownloadPath(localPath, shortName, remotePath string, remoteIsDir bool) string {
	podDir := filepath.Join(localPath, shortName)
	if remoteIsDir {
		return podDir
	}
	return filepath.Join(podDir, filepath.Base(remotePath))
}

func downloadTargetPath(localPath, shortName, remotePath string, remoteIsDir bool, podCount int) string {
	if podCount == 1 {
		return localPath
	}
	return multiPodDownloadPath(localPath, shortName, remotePath, remoteIsDir)
}

func resolveDownloadTargetPath(localPath, shortName, remotePath string, remoteIsDir bool, podCount int) (string, error) {
	targetPath := downloadTargetPath(localPath, shortName, remotePath, remoteIsDir, podCount)
	if remoteIsDir || podCount != 1 {
		return targetPath, nil
	}

	info, err := os.Stat(targetPath)
	switch {
	case err == nil && info.IsDir():
		return filepath.Join(targetPath, filepath.Base(remotePath)), nil
	case err == nil:
		return targetPath, nil
	case os.IsNotExist(err):
		return targetPath, nil
	default:
		return "", err
	}
}

func downloadSuccessDestination(localPath, shortName string, podCount int) string {
	if podCount == 1 {
		return localPath
	}
	return filepath.Join(localPath, shortName) + string(os.PathSeparator)
}

type cpResult struct {
	pod string
	err error
}

// singlePodCpMessage returns the prefix used in the transient progress line
// for a single-pod copy. Uses an arrow to make direction obvious.
func singlePodCpMessage(localPath, remotePath string, upload bool) string {
	if upload {
		return fmt.Sprintf("Copying %s → :%s", localPath, remotePath)
	}
	return fmt.Sprintf("Copying :%s → %s", remotePath, localPath)
}

// singlePodCpDoneLine returns the persistent line printed after a successful
// single-pod copy. Format intentionally mirrors the per-pod success line
// printed by runMultiPodCp so users see consistent output across modes.
func singlePodCpDoneLine(localPath, remotePath string, upload bool) string {
	if upload {
		return fmt.Sprintf("Copied %s -> :%s", localPath, remotePath)
	}
	return fmt.Sprintf("Copied :%s -> %s", remotePath, localPath)
}

func runMultiPodCp(cmd *cobra.Command, cc *commandContext, localPath, remotePath string, upload bool, allPods bool, podNames []string, role string, labels []string, exclude []string, container string, fanout int, readyOnly bool) error {
	ctx := cmd.Context()
	labelSel := selectorForSessionRun(cc.sessionName)
	sessionPods, err := cc.kube.ListPods(ctx, cc.namespace, false, labelSel)
	if err != nil {
		return fmt.Errorf("list session pods: %w", err)
	}

	// Apply user-specified filters before the readiness check so the
	// running-vs-total comparison only considers targeted pods.
	filteredPods := sessionPods
	switch {
	case allPods:
		// keep all
	case len(podNames) > 0:
		filteredPods = filterPodsByName(filteredPods, podNames)
	case role != "":
		filteredPods = filterPodsByRole(filteredPods, role)
	case len(labels) > 0:
		filteredPods = filterPodsByLabels(filteredPods, labels)
	}
	if len(exclude) > 0 {
		filteredPods = excludePods(filteredPods, exclude)
	}

	if len(filteredPods) == 0 {
		return fmt.Errorf("no pods match the specified filters in session %q", cc.sessionName)
	}

	pods := filterRunningPods(filteredPods)

	if len(pods) == 0 {
		return fmt.Errorf("no running pods in session %q (0/%d pods ready)", cc.sessionName, len(filteredPods))
	}

	if len(pods) < len(filteredPods) && !readyOnly {
		notReady := make([]string, 0, len(filteredPods)-len(pods))
		runningSet := make(map[string]bool, len(pods))
		for _, p := range pods {
			runningSet[p.Name] = true
		}
		for _, p := range filteredPods {
			if !runningSet[p.Name] {
				notReady = append(notReady, fmt.Sprintf("%s (%s)", p.Name, p.Phase))
			}
		}
		return fmt.Errorf("%d/%d pods are not running: %s\nUse --ready-only to copy on the %d ready pods",
			len(notReady), len(filteredPods), strings.Join(notReady, ", "), len(pods))
	}

	targetContainer := container
	if targetContainer == "" {
		targetContainer = resolveTargetContainer(cc.cfg)
	}

	effectiveFanout := fanout
	if effectiveFanout <= 0 {
		effectiveFanout = pdshDefaultFanout
	}

	podNameList := make([]string, len(pods))
	for i, p := range pods {
		podNameList[i] = p.Name
	}
	shortNames := shortPodNames(podNameList)
	nameMap := make(map[string]string, len(podNameList))
	for i, n := range podNameList {
		nameMap[n] = shortNames[i]
	}

	rawOut := cmd.OutOrStdout()
	// Wrap output so the spinner re-render and per-pod completion lines
	// (written from goroutines draining `results`) cannot interleave.
	out := io.Writer(&syncWriter{w: rawOut})
	results := make(chan cpResult, len(pods))
	sem := make(chan struct{}, effectiveFanout)
	podCount := len(pods)

	prog := newMultiPodProgress(out, podCount)
	prog.start()
	defer prog.stop()

	var wg sync.WaitGroup
	for _, pod := range pods {
		wg.Add(1)
		go func(pod kube.PodSummary) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			short := nameMap[pod.Name]
			prog.startPod(short)
			defer prog.finishPod(short)

			var cpErr error
			if upload {
				cpErr = runSinglePodCpWithProgress(ctx, cc.kube, cc.namespace, pod.Name, targetContainer, localPath, remotePath, true, io.Discard, prog)
			} else {
				isRemoteDir, err := cc.kube.IsRemoteDir(ctx, cc.namespace, pod.Name, targetContainer, remotePath)
				if err != nil {
					results <- cpResult{pod: pod.Name, err: err}
					return
				}
				podLocalPath, err := resolveDownloadTargetPath(localPath, short, remotePath, isRemoteDir, podCount)
				if err != nil {
					results <- cpResult{pod: pod.Name, err: err}
					return
				}
				cpErr = runSinglePodCpWithProgress(ctx, cc.kube, cc.namespace, pod.Name, targetContainer, podLocalPath, remotePath, false, io.Discard, prog)
			}
			results <- cpResult{pod: pod.Name, err: cpErr}
		}(pod)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	var failures []cpResult
	for r := range results {
		short := nameMap[r.pod]
		if r.err != nil {
			prog.println(fmt.Sprintf("%s: error: %v", short, r.err))
			failures = append(failures, r)
		} else {
			if upload {
				prog.println(fmt.Sprintf("%s: copied %s -> :%s", short, localPath, remotePath))
			} else {
				prog.println(fmt.Sprintf("%s: copied :%s -> %s", short, remotePath, downloadSuccessDestination(localPath, short, podCount)))
			}
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("%d of %d pods failed", len(failures), len(pods))
	}
	return nil
}
