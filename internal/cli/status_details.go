package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/session"
	syncengine "github.com/acmore/okdev/internal/sync"
	corev1 "k8s.io/api/core/v1"
)

var (
	sshForwardStatusFunc = isManagedSSHForwardRunning
	sshConfigPresentFunc = managedSSHConfigPresent
)

type statusDetailsClient interface {
	DescribePod(context.Context, string, string) (string, error)
	GetPod(context.Context, string, string) (*corev1.Pod, error)
}

type detailedStatus struct {
	Session          string                        `json:"session"`
	Namespace        string                        `json:"namespace"`
	Owner            string                        `json:"owner"`
	Workload         string                        `json:"workload"`
	Phase            string                        `json:"phase"`
	Ready            string                        `json:"ready"`
	Restarts         int32                         `json:"restarts"`
	Reason           string                        `json:"reason"`
	Age              string                        `json:"age"`
	PodCount         int                           `json:"podCount"`
	Target           detailedStatusTarget          `json:"target"`
	Pods             []detailedStatusPod           `json:"pods"`
	SSH              detailedStatusSSH             `json:"ssh"`
	Sync             detailedStatusSync            `json:"sync"`
	PathSemantics    []detailedStatusPathSemantics `json:"pathSemantics,omitempty"`
	Agents           []agentListRow                `json:"agents,omitempty"`
	Logs             detailedStatusLogs            `json:"logs"`
	TargetPodDetails string                        `json:"targetPodDetails,omitempty"`
}

type detailedStatusTarget struct {
	PinnedPod         string `json:"pinnedPod,omitempty"`
	PinnedContainer   string `json:"pinnedContainer,omitempty"`
	SelectedPod       string `json:"selectedPod"`
	SelectedContainer string `json:"selectedContainer"`
}

type detailedStatusPod struct {
	Name            string                         `json:"name"`
	Role            string                         `json:"role,omitempty"`
	Attachable      bool                           `json:"attachable"`
	Phase           string                         `json:"phase"`
	Ready           string                         `json:"ready"`
	Restarts        int32                          `json:"restarts"`
	Reason          string                         `json:"reason"`
	Age             string                         `json:"age"`
	Selected        bool                           `json:"selected"`
	PodIP           string                         `json:"podIP,omitempty"`
	NodeName        string                         `json:"nodeName,omitempty"`
	Mounts          []detailedStatusMount          `json:"mounts,omitempty"`
	ContainerIssues []detailedStatusContainerIssue `json:"containerIssues,omitempty"`
}

// detailedStatusContainerIssue surfaces an abnormal container termination
// (OOMKilled, Error, ...) so `ready=1/2` comes with its cause instead of
// requiring a kubectl round-trip through containerStatuses.
type detailedStatusContainerIssue struct {
	Container  string `json:"container"`
	Reason     string `json:"reason"`
	ExitCode   int32  `json:"exitCode"`
	FinishedAt string `json:"finishedAt,omitempty"`
	Current    bool   `json:"current"`
}

type detailedStatusMount struct {
	Name        string `json:"name"`
	MountPath   string `json:"mountPath"`
	ReadOnly    bool   `json:"readOnly,omitempty"`
	SourceType  string `json:"sourceType"`
	Persistence string `json:"persistence"`
}

type detailedStatusPathSemantics struct {
	LocalPath     string `json:"localPath"`
	RemotePath    string `json:"remotePath"`
	WorkspacePath string `json:"workspacePath"`
	SyncScope     string `json:"syncScope"`
	Survival      string `json:"survival"`
}

type detailedStatusPathPair struct {
	LocalPath  string
	RemotePath string
}

type detailedStatusSSH struct {
	HostAlias            string   `json:"hostAlias"`
	ControlSocket        string   `json:"controlSocket,omitempty"`
	ConfigPresent        bool     `json:"configPresent"`
	ManagedForwardStatus string   `json:"managedForwardStatus"`
	ConfiguredForwards   []string `json:"configuredForwards,omitempty"`
}

type detailedStatusSync struct {
	Engine             string             `json:"engine"`
	Direction          string             `json:"direction,omitempty"`
	Health             string             `json:"health,omitempty"`
	HealthDetail       string             `json:"healthDetail,omitempty"`
	ConfiguredPaths    []string           `json:"configuredPaths,omitempty"`
	BackgroundStatus   string             `json:"backgroundStatus,omitempty"`
	BackgroundPID      int                `json:"backgroundPid,omitempty"`
	PIDPath            string             `json:"pidPath,omitempty"`
	LogPath            string             `json:"logPath,omitempty"`
	LocalHome          string             `json:"localHome,omitempty"`
	LocalDaemonLogPath string             `json:"localDaemonLogPath,omitempty"`
	ConflictCount      int                `json:"conflictCount,omitempty"`
	ConflictPaths      []string           `json:"conflictPaths,omitempty"`
	MeshLines          []string           `json:"meshLines,omitempty"`
	MeshHealth         *meshHealthSummary `json:"meshHealth,omitempty"`
}

type detailedStatusLogs struct {
	OKDevLog string `json:"okdevLog,omitempty"`
}

func gatherDetailedStatus(ctx context.Context, opts *Options, cfg *config.DevEnvironment, cfgPath, namespace string, view sessionView, client statusDetailsClient) detailedStatus {
	detail := detailedStatus{
		Session:   view.Session,
		Namespace: view.Namespace,
		Owner:     view.Owner,
		Workload:  view.WorkloadType,
		Phase:     view.Phase,
		Ready:     view.Ready,
		Restarts:  view.Restarts,
		Reason:    view.Reason,
		Age:       age(view.CreatedAt),
		PodCount:  view.PodCount,
	}

	target, pods := buildDetailedTarget(view, cfg)
	detail.Target = target
	detail.Pods = pods
	detail.SSH = buildDetailedSSH(view.Session, cfg.Spec.Ports)
	detail.Sync = buildDetailedSync(view.Session, cfg, cfgPath)
	detail.Sync.MeshLines = buildMeshLinesFromPods(view.Pods)
	if kubeClient, ok := client.(*kube.Client); ok && opts != nil {
		if liveMesh := probeLiveMeshHealth(ctx, opts, kubeClient, namespace, view); liveMesh != nil {
			detail.Sync.MeshHealth = liveMesh
		}
	}
	if agentClient, ok := client.(agentExecClient); ok && len(cfg.Spec.Agents) > 0 && strings.TrimSpace(view.TargetPod) != "" && strings.TrimSpace(target.SelectedContainer) != "" {
		if rows, err := configuredAgentStatusRows(ctx, agentClient, namespace, view.TargetPod, target.SelectedContainer, cfg.Spec.Agents); err == nil {
			detail.Agents = rows
		}
	}
	detail.Logs = buildDetailedLogs()
	enrichDetailedStatusPods(ctx, namespace, target.SelectedContainer, &detail, client)
	detail.PathSemantics = buildDetailedPathSemantics(cfg, cfgPath, detail.Pods, target)

	if client != nil && strings.TrimSpace(view.TargetPod) != "" {
		if desc, err := client.DescribePod(ctx, namespace, view.TargetPod); err == nil {
			detail.TargetPodDetails = desc
		}
	}

	return detail
}

func buildDetailedTarget(view sessionView, cfg *config.DevEnvironment) (detailedStatusTarget, []detailedStatusPod) {
	pinned, _ := session.LoadTarget(view.Session)
	selectedContainer := resolveTargetContainer(cfg)
	if strings.TrimSpace(pinned.Container) != "" && strings.TrimSpace(pinned.PodName) == strings.TrimSpace(view.TargetPod) {
		selectedContainer = strings.TrimSpace(pinned.Container)
	}

	target := detailedStatusTarget{
		PinnedPod:         strings.TrimSpace(pinned.PodName),
		PinnedContainer:   strings.TrimSpace(pinned.Container),
		SelectedPod:       view.TargetPod,
		SelectedContainer: selectedContainer,
	}

	pods := make([]detailedStatusPod, 0, len(view.Pods))
	for _, pod := range sortedSessionPods(view.Pods) {
		issues := make([]detailedStatusContainerIssue, 0, len(pod.ContainerIssues))
		for _, issue := range pod.ContainerIssues {
			finishedAt := ""
			if !issue.FinishedAt.IsZero() {
				finishedAt = issue.FinishedAt.UTC().Format(time.RFC3339)
			}
			issues = append(issues, detailedStatusContainerIssue{
				Container:  issue.Container,
				Reason:     issue.Reason,
				ExitCode:   issue.ExitCode,
				FinishedAt: finishedAt,
				Current:    issue.Current,
			})
		}
		pods = append(pods, detailedStatusPod{
			Name:            pod.Name,
			Role:            strings.TrimSpace(pod.Labels["okdev.io/workload-role"]),
			Attachable:      !strings.EqualFold(strings.TrimSpace(pod.Labels["okdev.io/attachable"]), "false"),
			Phase:           pod.Phase,
			Ready:           pod.Ready,
			Restarts:        pod.Restarts,
			Reason:          pod.Reason,
			Age:             age(pod.CreatedAt),
			Selected:        pod.Name == view.TargetPod,
			PodIP:           pod.PodIP,
			NodeName:        pod.NodeName,
			ContainerIssues: issues,
		})
	}
	return target, pods
}

func enrichDetailedStatusPods(ctx context.Context, namespace, container string, detail *detailedStatus, client statusDetailsClient) {
	if detail == nil || client == nil {
		return
	}
	for i := range detail.Pods {
		pod, err := client.GetPod(ctx, namespace, detail.Pods[i].Name)
		if err != nil || pod == nil {
			continue
		}
		if strings.TrimSpace(detail.Pods[i].PodIP) == "" {
			detail.Pods[i].PodIP = strings.TrimSpace(pod.Status.PodIP)
		}
		if strings.TrimSpace(detail.Pods[i].NodeName) == "" {
			detail.Pods[i].NodeName = strings.TrimSpace(pod.Spec.NodeName)
		}
		detail.Pods[i].Mounts = podMountsForContainer(pod, container)
	}
}

func buildDetailedPathSemantics(cfg *config.DevEnvironment, cfgPath string, pods []detailedStatusPod, target detailedStatusTarget) []detailedStatusPathSemantics {
	if cfg == nil {
		return nil
	}
	pairs, err := parseDetailedStatusPathPairs(cfg, cfgPath)
	if err != nil || len(pairs) == 0 {
		return nil
	}
	workspacePath := cfg.EffectiveWorkspaceMountPath(cfgPath)
	if workspacePath == "" {
		workspacePath = "/workspace"
	}
	for _, pod := range pods {
		if pod.Name == target.SelectedPod {
			return buildPathSemantics(pairs, pod.Mounts, workspacePath)
		}
	}
	return buildPathSemantics(pairs, nil, workspacePath)
}

func parseDetailedStatusPathPairs(cfg *config.DevEnvironment, cfgPath string) ([]detailedStatusPathPair, error) {
	pairs, err := syncengine.ParsePairs(cfg.Spec.Sync.Paths, cfg.EffectiveWorkspaceMountPath(cfgPath))
	if err != nil {
		return nil, err
	}
	out := make([]detailedStatusPathPair, 0, len(pairs))
	for _, pair := range pairs {
		out = append(out, detailedStatusPathPair{LocalPath: pair.Local, RemotePath: pair.Remote})
	}
	return out, nil
}

func buildPathSemantics(pairs []detailedStatusPathPair, mounts []detailedStatusMount, workspacePath string) []detailedStatusPathSemantics {
	out := make([]detailedStatusPathSemantics, 0, len(pairs))
	for _, pair := range pairs {
		scope := "not-shared-by-sync"
		if pathWithin(pair.RemotePath, workspacePath) {
			scope = "all-pods-via-syncthing"
		}
		survival := "depends on workload storage"
		if mount, ok := findBestMountForPath(mounts, pair.RemotePath); ok {
			switch mount.Persistence {
			case "persistent":
				survival = "survives session restart"
			case "ephemeral":
				survival = "ephemeral in pod"
			}
		}
		out = append(out, detailedStatusPathSemantics{
			LocalPath:     pair.LocalPath,
			RemotePath:    pair.RemotePath,
			WorkspacePath: workspacePath,
			SyncScope:     scope,
			Survival:      survival,
		})
	}
	return out
}

func podMountsForContainer(pod *corev1.Pod, container string) []detailedStatusMount {
	if pod == nil {
		return nil
	}
	container = strings.TrimSpace(container)
	if container == "" && len(pod.Spec.Containers) > 0 {
		container = pod.Spec.Containers[0].Name
	}
	volumes := make(map[string]corev1.Volume, len(pod.Spec.Volumes))
	for _, volume := range pod.Spec.Volumes {
		volumes[volume.Name] = volume
	}
	for _, c := range pod.Spec.Containers {
		if c.Name != container {
			continue
		}
		mounts := make([]detailedStatusMount, 0, len(c.VolumeMounts))
		for _, mount := range c.VolumeMounts {
			sourceType := "other"
			persistence := "unknown"
			if volume, ok := volumes[mount.Name]; ok {
				sourceType, persistence = classifyMountSource(volume)
			}
			mounts = append(mounts, detailedStatusMount{
				Name:        mount.Name,
				MountPath:   mount.MountPath,
				ReadOnly:    mount.ReadOnly,
				SourceType:  sourceType,
				Persistence: persistence,
			})
		}
		return mounts
	}
	return nil
}

func classifyMountSource(volume corev1.Volume) (string, string) {
	switch {
	case volume.EmptyDir != nil:
		return "emptyDir", "ephemeral"
	case volume.PersistentVolumeClaim != nil:
		return "persistentVolumeClaim", "persistent"
	case volume.HostPath != nil:
		return "hostPath", "persistent"
	case volume.ConfigMap != nil:
		return "configMap", "ephemeral"
	case volume.Secret != nil:
		return "secret", "ephemeral"
	case volume.Projected != nil:
		return "projected", "ephemeral"
	case volume.DownwardAPI != nil:
		return "downwardAPI", "ephemeral"
	default:
		return "other", "unknown"
	}
}

func findBestMountForPath(mounts []detailedStatusMount, remotePath string) (detailedStatusMount, bool) {
	bestLen := -1
	var best detailedStatusMount
	for _, mount := range mounts {
		if !pathWithin(remotePath, mount.MountPath) {
			continue
		}
		if l := len(strings.TrimRight(mount.MountPath, "/")); l > bestLen {
			bestLen = l
			best = mount
		}
	}
	return best, bestLen >= 0
}

func pathWithin(path, root string) bool {
	path = strings.TrimSpace(path)
	root = strings.TrimSpace(root)
	if path == "" || root == "" {
		return false
	}
	cleanPath := filepath.Clean(path)
	cleanRoot := filepath.Clean(root)
	return cleanPath == cleanRoot || strings.HasPrefix(cleanPath, cleanRoot+string(os.PathSeparator))
}

func buildDetailedSSH(sessionName string, forwards []config.PortMapping) detailedStatusSSH {
	alias := sshHostAlias(sessionName)
	socketPath, socketErr := sshControlSocketStatusPath(alias)

	detail := detailedStatusSSH{
		HostAlias: alias,
	}
	if socketErr == nil {
		detail.ControlSocket = socketPath
	}

	configPresent, err := sshConfigPresentFunc(alias)
	if err != nil {
		detail.ManagedForwardStatus = "unknown (" + err.Error() + ")"
	} else {
		detail.ConfigPresent = configPresent
	}

	if running, runErr := sshForwardStatusFunc(alias); runErr != nil {
		detail.ManagedForwardStatus = "unknown (" + runErr.Error() + ")"
	} else if running {
		detail.ManagedForwardStatus = "running"
	} else if detail.ManagedForwardStatus == "" {
		detail.ManagedForwardStatus = "stopped"
	}

	detail.ConfiguredForwards = summarizeConfiguredPorts(forwards)
	return detail
}

func buildDetailedSync(sessionName string, cfg *config.DevEnvironment, cfgPath string) detailedStatusSync {
	engine := strings.TrimSpace(cfg.Spec.Sync.Engine)
	if engine == "" {
		engine = "syncthing"
	}
	direction := ""
	if pairs, err := syncengine.ParsePairs(cfg.Spec.Sync.Paths, cfg.EffectiveWorkspaceMountPath(cfgPath)); err == nil && len(pairs) > 0 {
		direction = pairs[0].Direction
	}
	detail := detailedStatusSync{
		Engine:          engine,
		Direction:       direction,
		ConfiguredPaths: summarizeConfiguredSyncPaths(cfg, cfgPath),
	}
	if engine != "syncthing" {
		return detail
	}

	pidPath, err := syncthingPIDStatusPath(sessionName)
	if err == nil {
		detail.PIDPath = pidPath
		if pid, ok := readSyncthingPID(pidPath); ok {
			detail.BackgroundPID = pid
			if processAlive(pid) && processLooksLikeSyncthingSync(pid) {
				detail.BackgroundStatus = "running"
			} else {
				detail.BackgroundStatus = "stale pid file"
			}
		} else {
			detail.BackgroundStatus = "not running"
		}
	}

	if home, err := localSyncthingStatusHome(sessionName); err == nil {
		detail.LocalHome = home
		if logPath, logErr := localSyncthingLogPath(home); logErr == nil {
			detail.LocalDaemonLogPath = logPath
		}
	}

	if logPath, err := syncthingBackgroundLogPath(sessionName); err == nil {
		detail.LogPath = logPath
	}
	if pairs, err := syncengine.ParsePairs(cfg.Spec.Sync.Paths, cfg.EffectiveWorkspaceMountPath(cfgPath)); err == nil {
		if count, paths := localSyncConflictStatus(pairs); count > 0 {
			detail.ConflictCount = count
			detail.ConflictPaths = paths
		}
	}

	health, reason := checkSyncHealth(sessionName)
	detail.Health = string(health)
	detail.HealthDetail = reason
	return detail
}

func buildMeshLinesFromPods(pods []kube.PodSummary) []string {
	var hub string
	var receivers []string
	for _, p := range pods {
		role := strings.TrimSpace(p.Labels["okdev.io/mesh-role"])
		switch role {
		case "hub":
			hub = p.Name
		case "receiver":
			receivers = append(receivers, p.Name)
		}
	}
	if hub == "" || len(receivers) == 0 {
		return nil
	}
	lines := []string{
		"topology: hub-and-spoke",
		fmt.Sprintf("hub: %s", hub),
		fmt.Sprintf("receivers: %d", len(receivers)),
	}
	for _, r := range receivers {
		lines = append(lines, fmt.Sprintf("  %s", r))
	}
	return lines
}

func probeLiveMeshHealth(ctx context.Context, opts *Options, k *kube.Client, namespace string, view sessionView) *meshHealthSummary {
	var hubPod string
	for _, p := range view.Pods {
		if strings.TrimSpace(p.Labels["okdev.io/mesh-role"]) == "hub" {
			hubPod = p.Name
			break
		}
	}
	if hubPod == "" {
		return nil
	}
	labels := map[string]string{
		"okdev.io/managed": "true",
		"okdev.io/session": view.Session,
	}
	folderID := "okdev-" + view.Session
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	summary, err := checkMeshHealth(probeCtx, opts, k, namespace, view.Session, labels, hubPod, folderID)
	if err != nil {
		slog.Debug("status: mesh health probe failed", "error", err)
		return nil
	}
	return summary
}

func buildDetailedLogs() detailedStatusLogs {
	home, err := os.UserHomeDir()
	if err != nil {
		return detailedStatusLogs{}
	}
	return detailedStatusLogs{
		OKDevLog: filepath.Join(home, ".okdev", "logs", "okdev.log"),
	}
}

func printDetailedStatus(w io.Writer, detail detailedStatus) {
	fmt.Fprintf(w, "Session: %s\n", detail.Session)
	fmt.Fprintf(w, "Namespace: %s\n", detail.Namespace)
	fmt.Fprintf(w, "Owner: %s\n", detail.Owner)
	fmt.Fprintf(w, "Workload: %s\n", detail.Workload)
	fmt.Fprintf(w, "Phase: %s\n", detail.Phase)
	fmt.Fprintf(w, "Ready: %s\n", detail.Ready)
	fmt.Fprintf(w, "Restarts: %d\n", detail.Restarts)
	if strings.TrimSpace(detail.Reason) != "" {
		fmt.Fprintf(w, "Reason: %s\n", detail.Reason)
	}
	fmt.Fprintf(w, "Age: %s\n", detail.Age)
	fmt.Fprintln(w, "\nTarget:")
	fmt.Fprintf(w, "- selected: %s/%s\n", detail.Target.SelectedPod, detail.Target.SelectedContainer)
	if detail.Target.PinnedPod != "" || detail.Target.PinnedContainer != "" {
		fmt.Fprintf(w, "- pinned: %s/%s\n", emptyDash(detail.Target.PinnedPod), emptyDash(detail.Target.PinnedContainer))
	}

	fmt.Fprintln(w, "\nPods:")
	for _, pod := range detail.Pods {
		marker := " "
		if pod.Selected {
			marker = "*"
		}
		line := fmt.Sprintf("%s %s phase=%s ready=%s restarts=%d age=%s", marker, pod.Name, pod.Phase, pod.Ready, pod.Restarts, pod.Age)
		if pod.Role != "" {
			line += " role=" + pod.Role
		}
		if pod.Reason != "" {
			line += " reason=" + pod.Reason
		}
		if !pod.Attachable {
			line += " attachable=false"
		}
		fmt.Fprintln(w, line)
		for _, extra := range []string{
			renderPodLocationLine("ip", pod.PodIP),
			renderPodLocationLine("node", pod.NodeName),
			renderMountsLine(pod.Mounts),
		} {
			if extra != "" {
				fmt.Fprintf(w, "  %s\n", extra)
			}
		}
		for _, issue := range pod.ContainerIssues {
			fmt.Fprintf(w, "  %s\n", renderContainerIssueLine(issue))
		}
	}

	fmt.Fprintln(w, "\nSSH:")
	fmt.Fprintf(w, "- host alias: %s\n", detail.SSH.HostAlias)
	fmt.Fprintf(w, "- config present: %t\n", detail.SSH.ConfigPresent)
	if detail.SSH.ControlSocket != "" {
		fmt.Fprintf(w, "- control socket: %s\n", detail.SSH.ControlSocket)
	}
	fmt.Fprintf(w, "- managed forward: %s\n", detail.SSH.ManagedForwardStatus)
	if len(detail.SSH.ConfiguredForwards) > 0 {
		fmt.Fprintln(w, "- configured forwards:")
		for _, forward := range detail.SSH.ConfiguredForwards {
			fmt.Fprintf(w, "  - %s\n", forward)
		}
	}

	fmt.Fprintln(w, "\nSync:")
	fmt.Fprintf(w, "- engine: %s\n", detail.Sync.Engine)
	if detail.Sync.Direction != "" {
		fmt.Fprintf(w, "- direction: %s (%s)\n", detail.Sync.Direction, modeSymbol(detail.Sync.Direction))
	}
	if detail.Sync.BackgroundStatus != "" {
		status := detail.Sync.BackgroundStatus
		if detail.Sync.BackgroundPID > 0 {
			status += " (pid " + strconv.Itoa(detail.Sync.BackgroundPID) + ")"
		}
		fmt.Fprintf(w, "- background: %s\n", status)
	}
	if detail.Sync.Health != "" {
		healthLine := detail.Sync.Health
		if detail.Sync.HealthDetail != "" {
			healthLine += " (" + detail.Sync.HealthDetail + ")"
		}
		fmt.Fprintf(w, "- health: %s\n", healthLine)
	}
	if len(detail.Sync.ConfiguredPaths) > 0 {
		fmt.Fprintln(w, "- configured paths:")
		for _, path := range detail.Sync.ConfiguredPaths {
			fmt.Fprintf(w, "  - %s\n", path)
		}
	}
	for _, line := range []struct {
		label string
		value string
	}{
		{"pid path", detail.Sync.PIDPath},
		{"background log", detail.Sync.LogPath},
		{"local home", detail.Sync.LocalHome},
		{"local daemon log", detail.Sync.LocalDaemonLogPath},
	} {
		if line.value != "" {
			fmt.Fprintf(w, "- %s: %s\n", line.label, line.value)
		}
	}
	if detail.Sync.ConflictCount > 0 {
		fmt.Fprintf(w, "- conflicts: %d\n", detail.Sync.ConflictCount)
		for _, path := range detail.Sync.ConflictPaths {
			fmt.Fprintf(w, "  - %s\n", path)
		}
	}

	if len(detail.Sync.MeshLines) > 0 || detail.Sync.MeshHealth != nil {
		fmt.Fprintln(w, "\nMesh:")
		for _, line := range detail.Sync.MeshLines {
			fmt.Fprintf(w, "- %s\n", line)
		}
		if detail.Sync.MeshHealth != nil {
			printMeshHealth(w, detail.Sync.MeshHealth)
		}
	}

	if len(detail.PathSemantics) > 0 {
		fmt.Fprintln(w, "\nPath Semantics:")
		for _, item := range detail.PathSemantics {
			fmt.Fprintf(w, "- %s -> %s sync=%s survival=%s\n", emptyDash(item.LocalPath), emptyDash(item.RemotePath), item.SyncScope, item.Survival)
		}
	}

	if len(detail.Agents) > 0 {
		fmt.Fprintln(w, "\nAgents:")
		for _, agent := range detail.Agents {
			installed := "no"
			if agent.Installed {
				installed = "yes"
			}
			authStaged := "no"
			if agent.AuthStaged {
				authStaged = "yes"
			}
			fmt.Fprintf(w, "- %s: installed=%s authStaged=%s\n", agent.Name, installed, authStaged)
		}
	}

	fmt.Fprintln(w, "\nLogs:")
	if detail.Logs.OKDevLog != "" {
		fmt.Fprintf(w, "- okdev: %s\n", detail.Logs.OKDevLog)
	}

	if strings.TrimSpace(detail.TargetPodDetails) != "" {
		fmt.Fprintln(w, "\nTarget Pod Details:")
		writeIndentedBlock(w, detail.TargetPodDetails, "  ")
	}
}

func renderPodLocationLine(label, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return label + "=" + value
}

func renderMountsLine(mounts []detailedStatusMount) string {
	if len(mounts) == 0 {
		return ""
	}
	parts := make([]string, 0, len(mounts))
	for _, mount := range mounts {
		parts = append(parts, fmt.Sprintf("%s [%s, %s]", mount.MountPath, mount.SourceType, mount.Persistence))
	}
	return "mounts: " + strings.Join(parts, ", ")
}

// renderContainerIssueLine formats one abnormal container termination, e.g.
// "container pytorch: OOMKilled (exit 137, finished 2026-07-05T06:32:11Z)".
func renderContainerIssueLine(issue detailedStatusContainerIssue) string {
	line := fmt.Sprintf("container %s: %s (exit %d", issue.Container, issue.Reason, issue.ExitCode)
	if issue.FinishedAt != "" {
		line += ", finished " + issue.FinishedAt
	}
	line += ")"
	if !issue.Current {
		line += " before last restart"
	}
	return line
}

func summarizeConfiguredPorts(forwards []config.PortMapping) []string {
	lines := make([]string, 0, len(forwards))
	for _, forward := range forwards {
		if summary, ok := portMappingSummary(forward); ok {
			lines = append(lines, summary)
		}
	}
	return lines
}

func summarizeConfiguredSyncPaths(cfg *config.DevEnvironment, cfgPath string) []string {
	pairs, err := syncengine.ParsePairs(cfg.Spec.Sync.Paths, cfg.EffectiveWorkspaceMountPath(cfgPath))
	if err != nil {
		return []string{"invalid sync config: " + err.Error()}
	}
	lines := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		lines = append(lines, fmt.Sprintf("%s %s %s", displayLocalSyncPath(pair.Local), syncArrow(pairModeSymbol(pair)), pair.Remote))
	}
	return lines
}

func localSyncConflictStatus(pairs []syncengine.Pair) (int, []string) {
	var count int
	samples := make([]string, 0, 3)
	for _, pair := range pairs {
		localPath := strings.TrimSpace(pair.Local)
		if localPath == "" {
			continue
		}
		absPath, err := filepath.Abs(localPath)
		if err != nil {
			continue
		}
		_ = filepath.WalkDir(absPath, func(path string, d os.DirEntry, err error) error {
			if err != nil || d == nil || d.IsDir() {
				return nil
			}
			if !looksLikeSyncthingConflict(path) {
				return nil
			}
			count++
			if len(samples) < 3 {
				if rel, relErr := filepath.Rel(absPath, path); relErr == nil {
					samples = append(samples, filepath.Join(localPath, rel))
				} else {
					samples = append(samples, path)
				}
			}
			return nil
		})
	}
	return count, samples
}

func looksLikeSyncthingConflict(path string) bool {
	return strings.Contains(strings.ToLower(filepath.Base(path)), ".sync-conflict-")
}

func syncthingBackgroundLogPath(sessionName string) (string, error) {
	home, err := session.SyncthingDir(sessionName)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "local.log"), nil
}

func sshControlSocketStatusPath(hostAlias string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".okdev", "ssh", hostAlias+".sock"), nil
}

func syncthingPIDStatusPath(sessionName string) (string, error) {
	home, err := session.SyncthingDir(sessionName)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "sync.pid"), nil
}

func localSyncthingStatusHome(sessionName string) (string, error) {
	return session.SyncthingDir(sessionName)
}

func managedSSHConfigPresent(hostAlias string) (bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return false, err
	}
	configPath := filepath.Join(home, ".ssh", "config")
	content, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return strings.Contains(string(content), "# BEGIN OKDEV "+hostAlias), nil
}

func writeIndentedBlock(w io.Writer, content, indent string) {
	for _, line := range strings.Split(strings.TrimRight(content, "\n"), "\n") {
		fmt.Fprintf(w, "%s%s\n", indent, line)
	}
}

func printMeshHealth(w io.Writer, health *meshHealthSummary) {
	if health == nil {
		return
	}
	healthy := 0
	for _, r := range health.Receivers {
		if r.Err == "" && r.Connected && r.InSync {
			healthy++
		}
	}
	total := len(health.Receivers)
	fmt.Fprintf(w, "- live health: %d/%d receiver(s) healthy\n", healthy, total)
	for _, r := range health.Receivers {
		state := "synced"
		if r.Err != "" {
			state = "error: " + r.Err
		} else if !r.Connected {
			state = "disconnected"
		} else if !r.InSync {
			state = fmt.Sprintf("syncing (%d bytes remaining)", r.NeedBytes)
		}
		fmt.Fprintf(w, "  %s: %s\n", r.Pod, state)
	}
}

func emptyDash(v string) string {
	if strings.TrimSpace(v) == "" {
		return "-"
	}
	return v
}
