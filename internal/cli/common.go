package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/connect"
	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/session"
	"github.com/acmore/okdev/internal/workload"
	"golang.org/x/term"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
)

var invalidOwnerChars = regexp.MustCompile(`[^a-z0-9._-]`)

type sessionResolver func(*Options, *config.DevEnvironment, string) (string, error)

type commandContext struct {
	opts        *Options
	cfg         *config.DevEnvironment
	cfgPath     string
	namespace   string
	sessionName string
	kube        *kube.Client
}

func loadConfigAndNamespace(opts *Options) (*config.DevEnvironment, string, error) {
	path, err := config.ResolvePath(opts.ConfigPath)
	if err != nil {
		return nil, "", err
	}
	done := announceConfigPath(path)
	cfg, path, err := config.Load(path)
	done(err == nil)
	if err != nil {
		var migErr *config.MigrationEligibleError
		if errors.As(err, &migErr) {
			fmt.Fprintf(os.Stderr, "\nHint: run \"okdev migrate\" to automatically fix this.\n")
		}
		return nil, "", err
	}
	applyConfigKubeContext(opts, cfg)
	ns := cfg.Spec.Namespace
	if opts.Namespace != "" {
		ns = opts.Namespace
	}
	if strings.TrimSpace(ns) == "" {
		ns = "default"
	}
	return cfg, ns, nil
}

func resolveCommandContext(opts *Options, resolver sessionResolver) (*commandContext, error) {
	effectiveOpts, err := optionsWithSessionConfig(opts)
	if err != nil {
		return nil, err
	}
	cfg, namespace, err := loadConfigAndNamespace(effectiveOpts)
	if err != nil {
		return nil, err
	}
	cfgPath, err := config.ResolvePath(effectiveOpts.ConfigPath)
	if err != nil {
		return nil, err
	}
	effectiveOpts.ConfigPath = cfgPath
	cc := &commandContext{
		opts:      effectiveOpts,
		cfg:       cfg,
		cfgPath:   cfgPath,
		namespace: namespace,
		kube:      newKubeClient(effectiveOpts),
	}
	if resolver == nil {
		return cc, nil
	}
	sessionName, err := resolver(effectiveOpts, cfg, namespace)
	if err != nil {
		return nil, err
	}
	cc.sessionName = sessionName
	return cc, nil
}

func optionsWithSessionConfig(opts *Options) (*Options, error) {
	if opts == nil {
		return &Options{}, nil
	}
	cloned := *opts
	if strings.TrimSpace(cloned.ConfigPath) != "" || strings.TrimSpace(cloned.Session) == "" {
		return &cloned, nil
	}
	info, err := session.LoadInfo(cloned.Session)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(info.ConfigPath) == "" {
		return nil, fmt.Errorf("session %q has no saved config path; run from the repo or pass --config", cloned.Session)
	}
	cloned.ConfigPath = info.ConfigPath
	if strings.TrimSpace(cloned.Context) == "" && strings.TrimSpace(info.KubeContext) != "" {
		cloned.Context = info.KubeContext
	}
	if strings.TrimSpace(cloned.Namespace) == "" && strings.TrimSpace(info.Namespace) != "" {
		cloned.Namespace = info.Namespace
	}
	return &cloned, nil
}

func withQuietConfigAnnounce(fn func() error) error {
	prevQuiet, hadQuiet := os.LookupEnv("OKDEV_QUIET_CONFIG_ANNOUNCE")
	_ = os.Setenv("OKDEV_QUIET_CONFIG_ANNOUNCE", "1")
	defer func() {
		if hadQuiet {
			_ = os.Setenv("OKDEV_QUIET_CONFIG_ANNOUNCE", prevQuiet)
		} else {
			_ = os.Unsetenv("OKDEV_QUIET_CONFIG_ANNOUNCE")
		}
	}()
	return fn()
}

func applyConfigKubeContext(opts *Options, cfg *config.DevEnvironment) {
	if opts == nil || cfg == nil {
		return
	}
	if strings.TrimSpace(opts.Context) != "" {
		return
	}
	if ctx := strings.TrimSpace(cfg.Spec.KubeContext); ctx != "" {
		opts.Context = ctx
	}
}

func announceConfigPath(path string) func(success bool) {
	return announceConfigPathWithWriter(os.Stderr, path, isInteractiveWriter(os.Stderr))
}

func announceConfigPathWithWriter(w io.Writer, path string, interactive bool) func(success bool) {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("OKDEV_QUIET_CONFIG_ANNOUNCE")), "1") {
		return func(bool) {}
	}
	if strings.TrimSpace(path) == "" {
		return func(bool) {}
	}
	if status := newTransientStatusWithMode(w, "Loading config "+path, interactive); status.enabled {
		return func(bool) {
			status.stop()
		}
	}
	return func(success bool) {
		if success {
			fmt.Fprintf(w, "Using config: %s\n", path)
		}
	}
}

type transientStatus struct {
	w        io.Writer
	message  string
	enabled  bool
	stopCh   chan struct{}
	doneCh   chan struct{}
	mu       sync.Mutex
	stopOnce sync.Once
	started  time.Time
}

func newTransientStatus(w io.Writer, message string) *transientStatus {
	return newTransientStatusWithMode(w, message, isInteractiveWriter(w))
}

func startTransientStatus(w io.Writer, message string) func() {
	status := newTransientStatus(w, message)
	if !status.enabled {
		return func() {}
	}
	return func() {
		status.stop()
	}
}

func newTransientStatusWithMode(w io.Writer, message string, interactive bool) *transientStatus {
	s := &transientStatus{
		w:       w,
		message: strings.TrimSpace(message),
		started: time.Now(),
	}
	if s.message == "" || !interactive {
		return s
	}
	s.enabled = true
	s.stopCh = make(chan struct{})
	s.doneCh = make(chan struct{})
	go s.run(statusSpinnerInterval)
	return s
}

func (s *transientStatus) run(interval time.Duration) {
	defer close(s.doneCh)
	frames := []string{"|", "/", "-", `\`}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	frame := 0
	s.render(frames[frame])
	for {
		select {
		case <-ticker.C:
			frame = (frame + 1) % len(frames)
			s.render(frames[frame])
		case <-s.stopCh:
			s.clear()
			return
		}
	}
}

func (s *transientStatus) render(frame string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var line string
	elapsed := time.Since(s.started)
	if elapsed >= statusElapsedThreshold {
		line = fmt.Sprintf("%s %s (%s)", frame, s.message, elapsed.Round(time.Second))
	} else {
		line = fmt.Sprintf("%s %s", frame, s.message)
	}
	// Truncate to terminal width to prevent line wrapping, which breaks
	// in-place updates (carriage return only resets within the last line).
	if w := terminalWidth(); w > 0 && len(line) > w {
		line = line[:w]
	}
	fmt.Fprintf(s.w, "\r%s\033[K", line)
}

func terminalWidth() int {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 0
	}
	return w
}

func (s *transientStatus) update(message string) {
	if !s.enabled {
		return
	}
	trimmed := strings.TrimSpace(message)
	if trimmed == "" {
		return
	}
	s.mu.Lock()
	s.message = trimmed
	s.mu.Unlock()
}

func (s *transientStatus) resetElapsed() {
	s.mu.Lock()
	s.started = time.Now()
	s.mu.Unlock()
}

func (s *transientStatus) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprint(s.w, "\r\033[K")
}

// printAbove writes a persistent line above the in-place status, clearing the
// current spinner line first so the printed output is not garbled by the
// concurrent spinner re-render. The spinner picks up where it left off on the
// next render tick.
func (s *transientStatus) printAbove(line string) {
	if !s.enabled {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	fmt.Fprintf(s.w, "\r\033[K%s", line)
}

func (s *transientStatus) stop() {
	if !s.enabled {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopCh)
		<-s.doneCh
	})
}

func isTerminalWriter(w io.Writer) bool {
	return isTerminalFD(w)
}

func isInteractiveWriter(w io.Writer) bool {
	return isTerminalWriter(w) && ansiEnabled()
}

func isTerminalReader(r io.Reader) bool {
	return isTerminalFD(r)
}

func ansiEnabled() bool {
	if strings.TrimSpace(os.Getenv("NO_COLOR")) != "" {
		return false
	}
	return !strings.EqualFold(strings.TrimSpace(os.Getenv("TERM")), "dumb")
}

func isTerminalFD(v any) bool {
	f, ok := v.(interface{ Fd() uintptr })
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// applySessionArg uses the first positional argument as the session name
// when --session was not explicitly provided.
func applySessionArg(opts *Options, args []string) {
	if len(args) > 0 && strings.TrimSpace(args[0]) != "" && strings.TrimSpace(opts.Session) == "" {
		opts.Session = args[0]
	}
}

func resolveSessionName(opts *Options, cfg *config.DevEnvironment, namespace string) (string, error) {
	return resolveSessionNameWithState(opts, cfg, namespace, true)
}

func resolveSessionNameWithState(opts *Options, cfg *config.DevEnvironment, namespace string, inferExisting bool) (string, error) {
	return resolveSessionNameWithReader(opts, cfg, namespace, inferExisting, newKubeClient(opts))
}

func resolveSessionNameWithReader(opts *Options, cfg *config.DevEnvironment, namespace string, inferExisting bool, reader sessionAccessReader) (string, error) {
	if strings.TrimSpace(opts.Session) != "" {
		return session.Resolve(opts.Session, cfg.Spec.Session.DefaultNameTemplate)
	}
	active, err := session.LoadActiveSession()
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(active) != "" {
		resolvedActive, err := session.Resolve(active, cfg.Spec.Session.DefaultNameTemplate)
		if err != nil {
			return "", err
		}
		if !sessionMatchesConfigPath(resolvedActive, opts.ConfigPath) {
			resolvedActive = ""
		}
		if resolvedActive == "" {
			goto inferExistingSession
		}
		exists, existsErr := sessionPodExists(reader, namespace, resolvedActive)
		if existsErr == nil {
			if exists {
				return resolvedActive, nil
			}
			workloadExists, workloadErr := sessionWorkloadExists(reader, namespace, resolvedActive)
			if workloadErr == nil && workloadExists {
				return resolvedActive, nil
			}
			if workloadErr != nil {
				// Don't pin the user to a possibly-dead session on a transient
				// API failure; fall through to inference instead.
				slog.Debug("failed to verify active session workload", "session", resolvedActive, "namespace", namespace, "error", workloadErr)
			} else if clearErr := session.ClearActiveSession(); clearErr != nil {
				slog.Debug("failed to clear stale active session", "session", resolvedActive, "error", clearErr)
			}
		} else {
			slog.Debug("failed to verify active session pod", "session", resolvedActive, "namespace", namespace, "error", existsErr)
			return resolvedActive, nil
		}
	}
inferExistingSession:
	if inferExisting {
		inferred, err := inferExistingSessionForRepo(opts, cfg, namespace, reader)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(inferred) != "" {
			return inferred, nil
		}
	}
	return session.ResolveDefault(cfg.Spec.Session.DefaultNameTemplate)
}

func sessionMatchesConfigPath(sessionName, currentConfigPath string) bool {
	currentConfigPath = normalizeConfigPath(currentConfigPath)
	if currentConfigPath == "" || strings.TrimSpace(sessionName) == "" {
		return true
	}
	info, err := session.LoadInfo(sessionName)
	if err != nil {
		slog.Debug("failed to load session info for config match", "session", sessionName, "error", err)
		return true
	}
	savedConfigPath := normalizeConfigPath(info.ConfigPath)
	if savedConfigPath == "" {
		return true
	}
	return savedConfigPath == currentConfigPath
}

func normalizeConfigPath(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return ""
	}
	abs, err := filepath.Abs(trimmed)
	if err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(trimmed)
}

func sessionPodExists(k sessionAccessReader, namespace, sessionName string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), sessionExistsTimeout)
	defer cancel()
	pods, err := k.ListPods(ctx, namespace, false, "okdev.io/managed=true,okdev.io/session="+sessionName)
	if err == nil {
		return len(pods) > 0, nil
	}
	_, err = k.GetPodSummary(ctx, namespace, podName(sessionName))
	if err == nil {
		return true, nil
	}
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return false, err
}

func sessionWorkloadExists(k sessionAccessReader, namespace, sessionName string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), sessionExistsTimeout)
	defer cancel()
	// Live controllers (by label) are the authoritative check; try them first
	// so a missing/corrupt saved Info file doesn't block discovery.
	exists, err := liveControllerWorkloadExists(ctx, k, namespace, sessionName)
	if err != nil {
		return false, err
	}
	if exists {
		return true, nil
	}
	info, loadErr := session.LoadInfo(sessionName)
	if loadErr != nil {
		slog.Debug("failed to load saved session info while verifying workload", "session", sessionName, "error", loadErr)
		return false, nil
	}
	return sessionInfoWorkloadExists(ctx, k, namespace, info)
}

func listLiveControllerResources(ctx context.Context, k sessionAccessReader, namespace string, allNamespaces bool, labelSelector string) ([]kube.ResourceSummary, error) {
	out := make([]kube.ResourceSummary, 0)
	for _, w := range workload.ControllerWorkloads() {
		items, err := k.ListResources(ctx, namespace, allNamespaces, w.APIVersion, w.Kind, labelSelector)
		if err != nil {
			if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		out = append(out, items...)
	}
	return out, nil
}

func liveControllerWorkloadExists(ctx context.Context, k sessionAccessReader, namespace, sessionName string) (bool, error) {
	items, err := listLiveControllerResources(ctx, k, namespace, false, "okdev.io/managed=true,okdev.io/session="+sessionName)
	if err != nil {
		return false, err
	}
	return len(items) > 0, nil
}

func sessionInfoWorkloadExists(ctx context.Context, k sessionAccessReader, namespace string, info session.Info) (bool, error) {
	if strings.TrimSpace(info.WorkloadAPIVersion) == "" ||
		strings.TrimSpace(info.WorkloadKind) == "" ||
		strings.TrimSpace(info.WorkloadName) == "" {
		return false, nil
	}
	workloadNamespace := strings.TrimSpace(info.Namespace)
	if workloadNamespace == "" {
		workloadNamespace = namespace
	}
	exists, err := k.ResourceExists(ctx, workloadNamespace, info.WorkloadAPIVersion, info.WorkloadKind, info.WorkloadName)
	if err != nil {
		if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return exists, nil
}

func buildSavedSessionView(ctx context.Context, k sessionAccessReader, namespace string, info session.Info) (sessionView, bool, error) {
	if strings.TrimSpace(info.Name) == "" {
		return sessionView{}, false, nil
	}
	exists, err := sessionInfoWorkloadExists(ctx, k, namespace, info)
	if err != nil || !exists {
		return sessionView{}, false, err
	}
	workloadNamespace := strings.TrimSpace(info.Namespace)
	if workloadNamespace == "" {
		workloadNamespace = namespace
	}
	workloadType := strings.TrimSpace(info.WorkloadType)
	if workloadType == "" {
		workloadType = strings.ToLower(strings.TrimSpace(info.WorkloadKind))
	}
	createdAt := info.CreatedAt
	if createdAt.IsZero() {
		createdAt = info.LastUsedAt
	}
	return sessionView{
		Namespace:    workloadNamespace,
		Session:      info.Name,
		Owner:        info.Owner,
		WorkloadType: workloadType,
		Phase:        "Pending",
		Ready:        "0/0",
		Reason:       "waiting for workload pods",
		CreatedAt:    createdAt,
		PodCount:     0,
	}, true, nil
}

func savedSessionViews(ctx context.Context, k sessionAccessReader, namespace string, allNamespaces, allUsers bool, opts *Options) ([]sessionView, error) {
	infos, err := session.ListInfos()
	if err != nil {
		return nil, err
	}
	owner := currentOwner(opts)
	views := make([]sessionView, 0, len(infos))
	for _, info := range infos {
		if strings.TrimSpace(info.Owner) == "" {
			info.Owner = owner
		}
		if !allUsers && strings.TrimSpace(info.Owner) != owner {
			continue
		}
		if !allNamespaces && strings.TrimSpace(info.Namespace) != "" && strings.TrimSpace(info.Namespace) != namespace {
			continue
		}
		view, ok, err := buildSavedSessionView(ctx, k, namespace, info)
		if err != nil {
			return nil, err
		}
		if ok {
			views = append(views, view)
		}
	}
	sort.Slice(views, func(i, j int) bool {
		return views[i].CreatedAt.After(views[j].CreatedAt)
	})
	return views, nil
}

func inferExistingSessionForRepo(opts *Options, cfg *config.DevEnvironment, namespace string, reader sessionAccessReader) (string, error) {
	root, err := session.RepoRoot()
	if err != nil || strings.TrimSpace(root) == "" {
		return "", nil
	}
	repo := filepath.Base(root)
	if strings.TrimSpace(repo) == "" {
		return "", nil
	}
	label := []string{
		"okdev.io/managed=true",
		ownerLabelSelector(opts),
		"okdev.io/repo=" + repo,
	}
	if strings.TrimSpace(cfg.Metadata.Name) != "" {
		label = append(label, "okdev.io/name="+cfg.Metadata.Name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionExistsTimeout)
	defer cancel()
	pods, err := reader.ListPods(ctx, namespace, false, strings.Join(label, ","))
	if err != nil {
		return "", err
	}
	currentConfigPath := normalizeConfigPath(opts.ConfigPath)
	if len(pods) == 0 {
		items, err := listLiveControllerResources(ctx, reader, namespace, false, strings.Join(label, ","))
		if err != nil {
			return "", err
		}
		if len(items) > 0 {
			sort.Slice(items, func(i, j int) bool {
				iMatch := sessionMatchesConfigPath(strings.TrimSpace(items[i].Labels["okdev.io/session"]), currentConfigPath)
				jMatch := sessionMatchesConfigPath(strings.TrimSpace(items[j].Labels["okdev.io/session"]), currentConfigPath)
				if iMatch != jMatch {
					return iMatch
				}
				return items[i].CreatedAt.After(items[j].CreatedAt)
			})
			for _, item := range items {
				if sn := strings.TrimSpace(item.Labels["okdev.io/session"]); sn != "" {
					return sn, nil
				}
			}
		}
		infos, err := session.ListInfos()
		if err != nil {
			return "", err
		}
		candidates := make([]session.Info, 0, len(infos))
		for _, info := range infos {
			if strings.TrimSpace(info.RepoRoot) != root {
				continue
			}
			if owner := strings.TrimSpace(info.Owner); owner != "" && owner != currentOwner(opts) {
				continue
			}
			if strings.TrimSpace(info.Namespace) != "" && strings.TrimSpace(info.Namespace) != namespace {
				continue
			}
			candidates = append(candidates, info)
		}
		sort.Slice(candidates, func(i, j int) bool {
			iMatch := sessionMatchesConfigPath(candidates[i].Name, currentConfigPath)
			jMatch := sessionMatchesConfigPath(candidates[j].Name, currentConfigPath)
			if iMatch != jMatch {
				return iMatch
			}
			iTime := candidates[i].LastUsedAt
			if iTime.IsZero() {
				iTime = candidates[i].CreatedAt
			}
			jTime := candidates[j].LastUsedAt
			if jTime.IsZero() {
				jTime = candidates[j].CreatedAt
			}
			return iTime.After(jTime)
		})
		for _, info := range candidates {
			exists, err := sessionInfoWorkloadExists(ctx, reader, namespace, info)
			if err != nil {
				return "", err
			}
			if exists {
				return info.Name, nil
			}
		}
		return "", nil
	}
	sort.Slice(pods, func(i, j int) bool {
		iMatch := sessionMatchesConfigPath(sessionNameFromPodSummary(pods[i]), currentConfigPath)
		jMatch := sessionMatchesConfigPath(sessionNameFromPodSummary(pods[j]), currentConfigPath)
		if iMatch != jMatch {
			return iMatch
		}
		return pods[i].CreatedAt.After(pods[j].CreatedAt)
	})
	for _, p := range pods {
		sn := strings.TrimSpace(p.Labels["okdev.io/session"])
		if sn != "" {
			return sn, nil
		}
		if strings.HasPrefix(p.Name, "okdev-") {
			return strings.TrimPrefix(p.Name, "okdev-"), nil
		}
	}
	return "", nil
}

func labelsForSession(opts *Options, cfg *config.DevEnvironment, sessionName string) map[string]string {
	owner := currentOwner(opts)
	repo := "unknown"
	if root, err := session.RepoRoot(); err == nil && root != "" {
		repo = filepath.Base(root)
	}
	return map[string]string{
		"okdev.io/managed":       "true",
		"okdev.io/name":          cfg.Metadata.Name,
		"okdev.io/session":       sessionName,
		"okdev.io/owner":         owner,
		"okdev.io/repo":          repo,
		"okdev.io/workload-type": cfg.Spec.Workload.Type,
	}
}

func selectorForSessionRun(sessionName string) string {
	labels := map[string]string{
		"okdev.io/managed": "true",
		"okdev.io/session": sessionName,
	}
	if info, err := session.LoadInfo(sessionName); err == nil {
		if runID := strings.TrimSpace(info.RunID); runID != "" {
			labels["okdev.io/run-id"] = runID
		}
	}
	return workload.DiscoveryLabelSelector(labels)
}

func annotationsForSession(cfg *config.DevEnvironment) map[string]string {
	out := map[string]string{}
	if cfg.Spec.Session.TTLHours > 0 {
		out["okdev.io/ttl-hours"] = fmt.Sprintf("%d", cfg.Spec.Session.TTLHours)
	}
	if cfg.Spec.Session.IdleTimeoutMinutes > 0 {
		out["okdev.io/idle-timeout-minutes"] = fmt.Sprintf("%d", cfg.Spec.Session.IdleTimeoutMinutes)
	}
	return out
}

func podName(sessionName string) string {
	return "okdev-" + sessionName
}

func newKubeClient(opts *Options) *kube.Client {
	return &kube.Client{Context: opts.Context}
}

func defaultContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), defaultContextTimeout)
}

func interactiveContext() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

func runConnect(opts *Options, namespace string, target workload.TargetRef, command []string, tty bool) error {
	return runConnectWithClient(newKubeClient(opts), namespace, target, command, tty)
}

func runConnectWithClient(k *kube.Client, namespace string, target workload.TargetRef, command []string, tty bool) error {
	ctx, cancel := interactiveContext()
	defer cancel()
	slog.Debug("connect start", "namespace", namespace, "pod", target.PodName, "tty", tty)
	return connect.Run(ctx, k, namespace, target.PodName, command, tty, os.Stdin, os.Stdout, os.Stderr)
}

func startSessionMaintenance(opts *Options, namespace, sessionName string, out io.Writer, emitHeartbeat bool) func() {
	return startSessionMaintenanceWithClient(newKubeClient(opts), namespace, sessionName, out, emitHeartbeat)
}

func startSessionMaintenanceWithClient(k *kube.Client, namespace, sessionName string, out io.Writer, emitHeartbeat bool) func() {
	if !emitHeartbeat {
		return func() {}
	}
	target, err := loadTargetRef(sessionName)
	pod := podName(sessionName)
	if err == nil && strings.TrimSpace(target.PodName) != "" {
		pod = target.PodName
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)
		heartbeatTicker := time.NewTicker(sessionHeartbeatInterval)
		defer heartbeatTicker.Stop()

		doHeartbeat := func() {
			beatCtx, beatCancel := context.WithTimeout(context.Background(), heartbeatWriteTimeout)
			err := k.TouchPodActivity(beatCtx, namespace, pod)
			beatCancel()
			if err != nil {
				slog.Warn("session activity heartbeat failed", "namespace", namespace, "session", sessionName, "pod", pod, "error", err)
				fmt.Fprintf(out, "warning: failed to update session activity heartbeat: %v\n", err)
			}
		}

		doHeartbeat()
		for {
			select {
			case <-ctx.Done():
				return
			case <-heartbeatTicker.C:
				doHeartbeat()
			}
		}
	}()

	return func() {
		cancel()
		<-done
	}
}

func currentOwner(opts *Options) string {
	if opts != nil {
		if v := normalizeOwner(opts.Owner); v != "" {
			return v
		}
	}
	if v := normalizeOwner(os.Getenv("OKDEV_OWNER")); v != "" {
		return v
	}
	if v := normalizeOwner(os.Getenv("USER")); v != "" {
		return v
	}
	return "dev"
}

func normalizeOwner(v string) string {
	s := strings.ToLower(strings.TrimSpace(v))
	s = strings.ReplaceAll(s, " ", "-")
	s = invalidOwnerChars.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	return s
}

func ownerLabelSelector(opts *Options) string {
	return "okdev.io/owner=" + currentOwner(opts)
}

func ensureSessionOwnership(opts *Options, k *kube.Client, namespace, sessionName string) error {
	return ensureSessionAccess(opts, k, namespace, sessionName, false)
}

func ensureExistingSessionOwnership(opts *Options, k *kube.Client, namespace, sessionName string) error {
	return ensureSessionAccess(opts, k, namespace, sessionName, true)
}

type sessionAccessReader interface {
	GetPodSummary(context.Context, string, string) (*kube.PodSummary, error)
	ListPods(context.Context, string, bool, string) ([]kube.PodSummary, error)
	ListResources(context.Context, string, bool, string, string, string) ([]kube.ResourceSummary, error)
	ResourceExists(context.Context, string, string, string, string) (bool, error)
}

func ensureSessionAccess(opts *Options, k sessionAccessReader, namespace, sessionName string, requireExisting bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), sessionAccessTimeout)
	defer cancel()
	pods, err := k.ListPods(ctx, namespace, false, "okdev.io/managed=true,okdev.io/session="+sessionName)
	if err != nil {
		return err
	}
	if len(pods) == 0 {
		if requireExisting {
			return fmt.Errorf("session %q does not exist in namespace %q", sessionName, namespace)
		}
		return nil
	}
	sort.Slice(pods, func(i, j int) bool {
		return workload.ComparePodPriority(pods[i], pods[j])
	})
	pod := &pods[0]
	owner := currentOwner(opts)
	otherOwner := strings.TrimSpace(pod.Labels["okdev.io/owner"])
	if otherOwner == "" || otherOwner == owner {
		return nil
	}
	return fmt.Errorf("session %q is owned by %q (current owner: %q); set --owner %s", sessionName, otherOwner, owner, otherOwner)
}

func ensureCommand(name string) error {
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("required command %q not found in PATH", name)
	}
	return nil
}

func outputJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func age(ts time.Time) string {
	if ts.IsZero() {
		return "-"
	}
	d := time.Since(ts)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
