package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/session"
	syncengine "github.com/acmore/okdev/internal/sync"
)

var (
	sshForwardStatusFunc = isManagedSSHForwardRunning
	sshConfigPresentFunc = managedSSHConfigPresent
)

type statusDetailsClient interface {
	DescribePod(context.Context, string, string) (string, error)
}

type detailedStatus struct {
	Session          string               `json:"session"`
	Namespace        string               `json:"namespace"`
	Owner            string               `json:"owner"`
	Workload         string               `json:"workload"`
	Phase            string               `json:"phase"`
	Ready            string               `json:"ready"`
	Restarts         int32                `json:"restarts"`
	Reason           string               `json:"reason"`
	Age              string               `json:"age"`
	PodCount         int                  `json:"podCount"`
	Shareable        bool                 `json:"shareable"`
	Target           detailedStatusTarget `json:"target"`
	Pods             []detailedStatusPod  `json:"pods"`
	SSH              detailedStatusSSH    `json:"ssh"`
	Sync             detailedStatusSync   `json:"sync"`
	Logs             detailedStatusLogs   `json:"logs"`
	TargetPodDetails string               `json:"targetPodDetails,omitempty"`
}

type detailedStatusTarget struct {
	PinnedPod         string `json:"pinnedPod,omitempty"`
	PinnedContainer   string `json:"pinnedContainer,omitempty"`
	SelectedPod       string `json:"selectedPod"`
	SelectedContainer string `json:"selectedContainer"`
}

type detailedStatusPod struct {
	Name       string `json:"name"`
	Role       string `json:"role,omitempty"`
	Attachable bool   `json:"attachable"`
	Phase      string `json:"phase"`
	Ready      string `json:"ready"`
	Restarts   int32  `json:"restarts"`
	Reason     string `json:"reason"`
	Age        string `json:"age"`
	Selected   bool   `json:"selected"`
}

type detailedStatusSSH struct {
	HostAlias            string   `json:"hostAlias"`
	ControlSocket        string   `json:"controlSocket,omitempty"`
	ConfigPresent        bool     `json:"configPresent"`
	ManagedForwardStatus string   `json:"managedForwardStatus"`
	ConfiguredForwards   []string `json:"configuredForwards,omitempty"`
}

type detailedStatusSync struct {
	Engine             string   `json:"engine"`
	ConfiguredPaths    []string `json:"configuredPaths,omitempty"`
	BackgroundStatus   string   `json:"backgroundStatus,omitempty"`
	BackgroundPID      int      `json:"backgroundPid,omitempty"`
	PIDPath            string   `json:"pidPath,omitempty"`
	LogPath            string   `json:"logPath,omitempty"`
	LocalHome          string   `json:"localHome,omitempty"`
	LocalDaemonLogPath string   `json:"localDaemonLogPath,omitempty"`
}

type detailedStatusLogs struct {
	OKDevLog string `json:"okdevLog,omitempty"`
}

func gatherDetailedStatus(ctx context.Context, cfg *config.DevEnvironment, namespace string, view sessionView, client statusDetailsClient) detailedStatus {
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

	if selected, ok := selectedStatusPod(view); ok {
		detail.Shareable = isSessionShareable(selected)
	}

	target, pods := buildDetailedTarget(view, cfg)
	detail.Target = target
	detail.Pods = pods
	detail.SSH = buildDetailedSSH(view.Session, cfg.Spec.Ports)
	detail.Sync = buildDetailedSync(view.Session, cfg)
	detail.Logs = buildDetailedLogs()

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
	if strings.TrimSpace(pinned.Container) != "" {
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
		pods = append(pods, detailedStatusPod{
			Name:       pod.Name,
			Role:       strings.TrimSpace(pod.Labels["okdev.io/workload-role"]),
			Attachable: !strings.EqualFold(strings.TrimSpace(pod.Labels["okdev.io/attachable"]), "false"),
			Phase:      pod.Phase,
			Ready:      pod.Ready,
			Restarts:   pod.Restarts,
			Reason:     pod.Reason,
			Age:        age(pod.CreatedAt),
			Selected:   pod.Name == view.TargetPod,
		})
	}
	return target, pods
}

func buildDetailedSSH(sessionName string, forwards []config.PortMapping) detailedStatusSSH {
	alias := sshHostAlias(sessionName)
	socketPath, socketErr := sshControlSocketPath(alias)

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

func buildDetailedSync(sessionName string, cfg *config.DevEnvironment) detailedStatusSync {
	engine := strings.TrimSpace(cfg.Spec.Sync.Engine)
	if engine == "" {
		engine = "syncthing"
	}
	detail := detailedStatusSync{
		Engine:          engine,
		ConfiguredPaths: summarizeConfiguredSyncPaths(cfg),
	}
	if engine != "syncthing" {
		return detail
	}

	pidPath, err := syncthingPIDPath(sessionName)
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

	if home, err := localSyncthingHome(sessionName); err == nil {
		detail.LocalHome = home
		if logPath, logErr := localSyncthingLogPath(home); logErr == nil {
			detail.LocalDaemonLogPath = logPath
		}
	}

	if logPath, err := syncthingBackgroundLogPath(sessionName); err == nil {
		detail.LogPath = logPath
	}
	return detail
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
	if detail.Shareable {
		fmt.Fprintln(w, "Shareable: true")
	}

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
	if detail.Sync.BackgroundStatus != "" {
		status := detail.Sync.BackgroundStatus
		if detail.Sync.BackgroundPID > 0 {
			status += " (pid " + strconv.Itoa(detail.Sync.BackgroundPID) + ")"
		}
		fmt.Fprintf(w, "- background: %s\n", status)
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

	fmt.Fprintln(w, "\nLogs:")
	if detail.Logs.OKDevLog != "" {
		fmt.Fprintf(w, "- okdev: %s\n", detail.Logs.OKDevLog)
	}

	if strings.TrimSpace(detail.TargetPodDetails) != "" {
		fmt.Fprintln(w, "\nTarget Pod Details:")
		writeIndentedBlock(w, detail.TargetPodDetails, "  ")
	}
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

func summarizeConfiguredSyncPaths(cfg *config.DevEnvironment) []string {
	pairs, err := syncengine.ParsePairs(cfg.Spec.Sync.Paths, cfg.WorkspaceMountPath())
	if err != nil {
		return []string{"invalid sync config: " + err.Error()}
	}
	lines := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		lines = append(lines, fmt.Sprintf("%s -> %s", displayLocalSyncPath(pair.Local), pair.Remote))
	}
	return lines
}

func syncthingBackgroundLogPath(sessionName string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".okdev", "logs", "syncthing-"+sessionName+".log"), nil
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

func selectedStatusPod(view sessionView) (kube.PodSummary, bool) {
	for _, pod := range view.Pods {
		if pod.Name == view.TargetPod {
			return pod, true
		}
	}
	return kube.PodSummary{}, false
}

func writeIndentedBlock(w io.Writer, content, indent string) {
	for _, line := range strings.Split(strings.TrimRight(content, "\n"), "\n") {
		fmt.Fprintf(w, "%s%s\n", indent, line)
	}
}

func emptyDash(v string) string {
	if strings.TrimSpace(v) == "" {
		return "-"
	}
	return v
}
