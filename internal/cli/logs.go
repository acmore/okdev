package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/workload"
	"github.com/spf13/cobra"
)

type logsRequest struct {
	Container string
	All       bool
	Follow    bool
	Previous  bool
	TailLines *int64
	Since     time.Duration
}

type podLogsClient interface {
	PodContainerNames(context.Context, string, string) ([]string, error)
	StreamPodLogs(context.Context, string, string, kube.LogStreamOptions, io.Writer) error
}

func newLogsCmd(opts *Options) *cobra.Command {
	var (
		container string
		all       bool
		follow    bool
		tail      int64
		since     time.Duration
		previous  bool
	)

	cmd := &cobra.Command{
		Use:               "logs [session]",
		Short:             "Stream session container logs",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: sessionCompletionFunc(opts),
		RunE: func(cmd *cobra.Command, args []string) error {
			applySessionArg(opts, args)
			req, err := buildLogsRequest(container, all, follow, previous, tail, cmd.Flags().Changed("tail"), since)
			if err != nil {
				return err
			}

			cc, err := resolveCommandContext(opts, resolveSessionName)
			if err != nil {
				return err
			}
			if err := ensureExistingSessionOwnership(cc.opts, cc.kube, cc.namespace, cc.sessionName); err != nil {
				return err
			}
			target, err := resolveTargetRef(cmd.Context(), cc.opts, cc.cfg, cc.namespace, cc.sessionName, cc.kube)
			if err != nil {
				return err
			}

			stopMaintenance := startSessionMaintenanceWithClient(cc.kube, cc.namespace, cc.sessionName, cmd.OutOrStdout(), true)
			defer stopMaintenance()

			return runLogsCommand(cmd.Context(), cc.kube, cc.namespace, target, req, cmd.OutOrStdout())
		},
	}

	cmd.Flags().StringVar(&container, "container", "", "Container name to stream logs from")
	cmd.Flags().BoolVar(&all, "all", false, "Stream logs from all containers in the target pod")
	cmd.Flags().BoolVar(&follow, "follow", true, "Follow logs")
	cmd.Flags().Int64Var(&tail, "tail", 0, "Show the last N lines before streaming")
	cmd.Flags().DurationVar(&since, "since", 0, "Only return logs newer than the given duration")
	cmd.Flags().BoolVar(&previous, "previous", false, "Return logs from the previous container instance")
	return cmd
}

func buildLogsRequest(container string, all, follow, previous bool, tail int64, tailSet bool, since time.Duration) (logsRequest, error) {
	if all && strings.TrimSpace(container) != "" {
		return logsRequest{}, errors.New("--all and --container cannot be used together")
	}
	if tailSet && tail < 0 {
		return logsRequest{}, errors.New("--tail must be >= 0")
	}
	if since < 0 {
		return logsRequest{}, errors.New("--since must be >= 0")
	}

	req := logsRequest{
		Container: strings.TrimSpace(container),
		All:       all,
		Follow:    follow,
		Previous:  previous,
		Since:     since,
	}
	if tailSet {
		req.TailLines = &tail
	}
	return req, nil
}

func runLogsCommand(ctx context.Context, client podLogsClient, namespace string, target workload.TargetRef, req logsRequest, out io.Writer) error {
	logOpts := kube.LogStreamOptions{
		Follow:    req.Follow,
		Previous:  req.Previous,
		TailLines: req.TailLines,
		Since:     req.Since,
	}

	if req.All {
		containers, err := client.PodContainerNames(ctx, namespace, target.PodName)
		if err != nil {
			return err
		}
		if len(containers) == 0 {
			return fmt.Errorf("pod %q has no containers", target.PodName)
		}
		return streamAllContainerLogs(ctx, client, namespace, target.PodName, containers, logOpts, out)
	}

	logOpts.Container = resolveLogsContainer(req.Container, target)
	if strings.TrimSpace(logOpts.Container) == "" {
		return fmt.Errorf("no target container resolved for pod %q", target.PodName)
	}
	return client.StreamPodLogs(ctx, namespace, target.PodName, logOpts, out)
}

func resolveLogsContainer(container string, target workload.TargetRef) string {
	if resolved := strings.TrimSpace(container); resolved != "" {
		return resolved
	}
	return strings.TrimSpace(target.Container)
}

func streamAllContainerLogs(ctx context.Context, client podLogsClient, namespace, pod string, containers []string, opts kube.LogStreamOptions, out io.Writer) error {
	var (
		wg     sync.WaitGroup
		errCh  = make(chan error, len(containers))
		writer = &lockedWriter{w: out}
	)

	for _, container := range containers {
		container := container
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := streamContainerLogsWithPrefix(ctx, client, namespace, pod, container, opts, writer); err != nil {
				errCh <- fmt.Errorf("stream logs for container %q: %w", container, err)
			}
		}()
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return err
		}
	}
	return nil
}

func streamContainerLogsWithPrefix(ctx context.Context, client podLogsClient, namespace, pod, container string, opts kube.LogStreamOptions, out io.Writer) error {
	pr, pw := io.Pipe()
	streamErrCh := make(chan error, 1)
	go func() {
		logOpts := opts
		logOpts.Container = container
		err := client.StreamPodLogs(ctx, namespace, pod, logOpts, pw)
		_ = pw.CloseWithError(err)
		streamErrCh <- err
	}()

	scanner := bufio.NewScanner(pr)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if _, err := fmt.Fprintf(out, "[%s] %s\n", container, scanner.Text()); err != nil {
			_ = pr.CloseWithError(err)
			<-streamErrCh
			return err
		}
	}

	scanErr := scanner.Err()
	streamErr := <-streamErrCh
	if streamErr != nil {
		return streamErr
	}
	if scanErr != nil {
		return scanErr
	}
	return nil
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}
