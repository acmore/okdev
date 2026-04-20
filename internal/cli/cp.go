package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/shellutil"
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
	var parallel int
	var quiet bool

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

			effectiveParallel := clampParallel(parallel)
			if multiPod {
				return runMultiPodCp(cmd, cc, localPath, remotePath, upload, allPods, podNames, role, labels, exclude, container, fanout, readyOnly, effectiveParallel, quiet)
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

			prefix, total := buildSinglePodCpPrefix(cmd.Context(), cc.kube, cc.namespace, target.PodName, targetContainer, localPath, remotePath, upload)
			var copyOpts kube.CopyOptions
			var progress *cpProgress
			if !quiet {
				announceCpStart(cmd.ErrOrStderr(), upload, localPath, remotePath, target.PodName, total, 1)
				progress = newCpProgress(cmd.ErrOrStderr(), prefix, total)
				copyOpts.Progress = progress.kube()
			}
			cpErr := runSinglePodCpWithParallel(cmd.Context(), cc.kube, cc.namespace, target.PodName, targetContainer, localPath, remotePath, upload, effectiveParallel, copyOpts)
			if progress != nil {
				progress.stop()
			}
			if cpErr == nil && !quiet {
				if upload {
					fmt.Fprintf(cmd.OutOrStdout(), "copied %s -> :%s (%s)\n", localPath, remotePath, progress.summary())
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "copied :%s -> %s (%s)\n", remotePath, localPath, progress.summary())
				}
			}
			return cpErr
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
	cmd.Flags().IntVar(&parallel, "parallel", cpDefaultParallel, "Parallel tar streams per pod for directory copies (default 1; raise only for high-latency/high-bandwidth links where a single stream under-utilizes the pipe)")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Suppress progress reporting and announce/summary lines")
	return cmd
}

const (
	// cpDefaultParallel keeps directory copies on a single exec stream by
	// default. Fanning out with explicit file lists trades one recursive tar
	// walk (fast path) for N walks with per-file stat overhead, and the
	// apiserver/kubelet is frequently the real bottleneck — so extra streams
	// can hurt. Users opt in explicitly when they know the pipe can use it.
	cpDefaultParallel = 1
	cpMaxParallel     = 16
	// cpMaxTotalStreams caps total concurrent exec streams against the API
	// server (fanout × parallel) in multi-pod mode.
	cpMaxTotalStreams = 32
)

// clampParallel keeps the --parallel value inside a safe range so a misread
// flag can't accidentally spin up hundreds of exec streams against the API
// server.
func clampParallel(v int) int {
	if v < 1 {
		return 1
	}
	if v > cpMaxParallel {
		return cpMaxParallel
	}
	return v
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

// announceCpStart prints a one-shot "about to transfer X" line so the user
// knows the target size and destination before the progress bar starts
// ticking. podCount describes how many pods will be touched (1 in single-pod
// mode, N for multi-pod). The announce goes to stderr alongside the progress
// line so stdout stays machine-friendly.
func announceCpStart(w io.Writer, upload bool, localPath, remotePath, pod string, perPodBytes int64, podCount int) {
	verb := "Downloading"
	src, dst := fmt.Sprintf(":%s", remotePath), localPath
	if upload {
		verb = "Uploading"
		src, dst = localPath, fmt.Sprintf(":%s", remotePath)
	}
	size := ""
	if perPodBytes > 0 {
		if podCount > 1 {
			size = fmt.Sprintf(" (%s × %d pods = %s)", humanBytes(perPodBytes), podCount, humanBytes(perPodBytes*int64(podCount)))
		} else {
			size = fmt.Sprintf(" (%s)", humanBytes(perPodBytes))
		}
	}
	via := ""
	if podCount == 1 && pod != "" {
		via = fmt.Sprintf(" via %s", pod)
	} else if podCount > 1 {
		via = fmt.Sprintf(" across %d pods", podCount)
	}
	fmt.Fprintf(w, "%s %s -> %s%s%s\n", verb, src, dst, size, via)
}

func runSinglePodCp(ctx context.Context, client *kube.Client, namespace, pod, container, localPath, remotePath string, upload bool, opts kube.CopyOptions) error {
	return runSinglePodCpWithParallel(ctx, client, namespace, pod, container, localPath, remotePath, upload, 1, opts)
}

// runSinglePodCpWithParallel is the work-horse copy path. When parallel > 1
// and the operation is a directory copy, it fans out onto
// CopyDir{To,From}PodParallelWithOptions. Single-file copies ignore parallel
// today; range-based single-file parallelism is left for a follow-up.
func runSinglePodCpWithParallel(ctx context.Context, client *kube.Client, namespace, pod, container, localPath, remotePath string, upload bool, parallel int, opts kube.CopyOptions) error {
	if upload {
		info, err := os.Stat(localPath)
		if err != nil {
			return err
		}
		if info.IsDir() {
			if parallel > 1 {
				return client.CopyDirToPodParallelWithOptions(ctx, namespace, pod, container, localPath, remotePath, parallel, opts)
			}
			return client.CopyDirToPodWithOptions(ctx, namespace, pod, container, localPath, remotePath, opts)
		}
		return client.CopyToPodInContainerWithOptions(ctx, namespace, localPath, pod, container, remotePath, opts)
	}

	isDir, err := client.IsRemoteDir(ctx, namespace, pod, container, remotePath)
	if err != nil {
		return err
	}
	if isDir {
		if err := os.MkdirAll(localPath, 0o755); err != nil {
			return err
		}
		if parallel > 1 {
			return client.CopyDirFromPodParallelWithOptions(ctx, namespace, pod, container, remotePath, localPath, parallel, opts)
		}
		return client.CopyDirFromPodWithOptions(ctx, namespace, pod, container, remotePath, localPath, opts)
	}
	return client.CopyFromPodInContainerWithOptions(ctx, namespace, pod, container, remotePath, localPath, opts)
}

// buildSinglePodCpPrefix returns a status-line prefix and a best-effort total
// byte count. Local sizes are computed via filepath.Walk; remote sizes fall
// back to 0 if probing the pod fails or the remote lacks `du`.
func buildSinglePodCpPrefix(ctx context.Context, client *kube.Client, namespace, pod, container, localPath, remotePath string, upload bool) (string, int64) {
	if upload {
		total := localPathSize(localPath)
		return fmt.Sprintf("→ %s", remotePath), total
	}
	total := remotePathSize(ctx, client, namespace, pod, container, remotePath)
	return fmt.Sprintf("← :%s", remotePath), total
}

// localPathSize sums the byte size of a local file or directory tree. Errors
// are swallowed because the total is only used for a nicer progress display.
func localPathSize(p string) int64 {
	info, err := os.Stat(p)
	if err != nil {
		return 0
	}
	if !info.IsDir() {
		return info.Size()
	}
	var total int64
	_ = filepath.Walk(p, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// remotePathSize best-effort asks the pod for the byte size of the path. It
// returns 0 if `du` is unavailable, the path is missing, or output can't be
// parsed.
func remotePathSize(ctx context.Context, client *kube.Client, namespace, pod, container, remotePath string) int64 {
	script := fmt.Sprintf("du -sb -- %s 2>/dev/null | awk '{print $1}'", shellutil.Quote(remotePath))
	out, err := client.ExecShInContainer(ctx, namespace, pod, container, script)
	if err != nil {
		return 0
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return 0
	}
	n, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
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

func runMultiPodCp(cmd *cobra.Command, cc *commandContext, localPath, remotePath string, upload bool, allPods bool, podNames []string, role string, labels []string, exclude []string, container string, fanout int, readyOnly bool, parallel int, quiet bool) error {
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

	// Keep total concurrent exec streams (fanout × parallel) under a soft cap
	// so a wide `--all` copy doesn't accidentally DoS the API server. Users who
	// really want higher concurrency can drop --fanout to compensate.
	effectiveParallel := parallel
	if effectiveParallel < 1 {
		effectiveParallel = 1
	}
	if effectiveFanout*effectiveParallel > cpMaxTotalStreams {
		effectiveParallel = cpMaxTotalStreams / effectiveFanout
		if effectiveParallel < 1 {
			effectiveParallel = 1
		}
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

	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()
	results := make(chan cpResult, len(pods))
	sem := make(chan struct{}, effectiveFanout)
	podCount := len(pods)

	// One aggregate progress line spans all pods; per-pod current-file events
	// would interleave meaninglessly so we only report aggregate bytes.
	var perPodTotal int64
	if upload {
		perPodTotal = localPathSize(localPath)
	} else if len(pods) > 0 {
		// Probe one running pod for the remote size so the aggregate total
		// has a sensible estimate. Assume all pods hold similarly-sized data.
		perPodTotal = remotePathSize(ctx, cc.kube, cc.namespace, pods[0].Name, targetContainer, remotePath)
	}
	totalBytes := perPodTotal * int64(podCount)
	var progress *cpProgress
	if !quiet {
		announceCpStart(errOut, upload, localPath, remotePath, "", perPodTotal, podCount)
		progress = newCpProgress(errOut, fmt.Sprintf("cp 0/%d pods", podCount), totalBytes)
		defer progress.stop()
	}

	var completed int32
	updatePodCount := func() {
		if progress == nil {
			return
		}
		done := atomic.LoadInt32(&completed)
		progress.setPrefix(fmt.Sprintf("cp %d/%d pods", done, podCount))
	}

	var wg sync.WaitGroup
	for _, pod := range pods {
		wg.Add(1)
		go func(pod kube.PodSummary) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			short := nameMap[pod.Name]
			var copyOpts kube.CopyOptions
			if progress != nil {
				copyOpts.Progress = progress.kubeBytesOnly()
			}
			var cpErr error
			if upload {
				cpErr = runSinglePodCpWithParallel(ctx, cc.kube, cc.namespace, pod.Name, targetContainer, localPath, remotePath, true, effectiveParallel, copyOpts)
			} else {
				isRemoteDir, err := cc.kube.IsRemoteDir(ctx, cc.namespace, pod.Name, targetContainer, remotePath)
				if err != nil {
					atomic.AddInt32(&completed, 1)
					updatePodCount()
					results <- cpResult{pod: pod.Name, err: err}
					return
				}
				podLocalPath := downloadTargetPath(localPath, short, remotePath, isRemoteDir, podCount)
				cpErr = runSinglePodCpWithParallel(ctx, cc.kube, cc.namespace, pod.Name, targetContainer, podLocalPath, remotePath, false, effectiveParallel, copyOpts)
			}
			atomic.AddInt32(&completed, 1)
			updatePodCount()
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
			fmt.Fprintf(out, "%s: error: %v\n", short, r.err)
			failures = append(failures, r)
		} else {
			if upload {
				fmt.Fprintf(out, "%s: copied %s -> :%s\n", short, localPath, remotePath)
			} else {
				fmt.Fprintf(out, "%s: copied :%s -> %s\n", short, remotePath, downloadSuccessDestination(localPath, short, podCount))
			}
		}
	}
	if progress != nil {
		progress.stop()
		fmt.Fprintf(out, "cp summary: %d/%d pods ok · %s\n", podCount-len(failures), podCount, progress.summary())
	} else {
		fmt.Fprintf(out, "cp summary: %d/%d pods ok\n", podCount-len(failures), podCount)
	}
	if len(failures) > 0 {
		return fmt.Errorf("%d of %d pods failed", len(failures), len(pods))
	}
	return nil
}
