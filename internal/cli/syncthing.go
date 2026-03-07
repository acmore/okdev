package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/acmore/okdev/internal/config"
	syncengine "github.com/acmore/okdev/internal/sync"
	"github.com/acmore/okdev/internal/syncthing"
	"github.com/spf13/cobra"
)

const syncthingContainerName = "syncthing"
const (
	syncthingLocalGUIAddr      = "127.0.0.1:8385"
	syncthingLocalAPIBase      = "http://127.0.0.1:8385"
	syncthingRemoteAPIBase     = "http://127.0.0.1:18384"
	syncthingPeerAddrTunnel    = "tcp://127.0.0.1:22000"
	syncthingPeerAddrDynamic   = "dynamic"
	syncthingAPIReadyTimeout   = 30 * time.Second
	syncthingLocalLogPath      = "/tmp/okdev-syncthing-local.log"
	syncthingHTTPClientTimeout = 15 * time.Second
)

var syncthingHTTPClient = &http.Client{Timeout: syncthingHTTPClientTimeout}

func runSyncthingSync(cmd *cobra.Command, opts *Options, cfg *config.DevEnvironment, namespace, sessionName, mode string, pairs []syncengine.Pair) error {
	if len(pairs) != 1 {
		return fmt.Errorf("syncthing engine currently supports exactly one sync path mapping")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	localBinary, err := syncthing.EnsureBinary(ctx, cfg.Spec.Sync.Syncthing.Version, cfg.Spec.Sync.Syncthing.AutoInstallEnabled())
	if err != nil {
		return fmt.Errorf("prepare local syncthing binary: %w", err)
	}

	k := newKubeClient(opts)
	pod := podName(sessionName)
	if _, err := k.ExecShInContainer(ctx, namespace, pod, syncthingContainerName, "command -v syncthing >/dev/null 2>&1"); err != nil {
		return fmt.Errorf("syncthing sidecar not ready; run `okdev up` with sync.engine=syncthing: %w", err)
	}

	pair := pairs[0]
	absLocal, err := filepath.Abs(pair.Local)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(absLocal, 0o755); err != nil {
		return err
	}
	if err := writeSTIgnore(absLocal, cfg.Spec.Sync.Exclude); err != nil {
		return err
	}
	if _, err := k.ExecShInContainer(ctx, namespace, pod, syncthingContainerName, fmt.Sprintf("mkdir -p %s", syncengine.ShellEscape(pair.Remote))); err != nil {
		return err
	}
	if err := writeRemoteSTIgnoreInPod(ctx, k, namespace, pod, pair.Remote, cfg.Spec.Sync.RemoteExclude); err != nil {
		return err
	}

	localHome, err := localSyncthingHome(sessionName)
	if err != nil {
		return err
	}
	if err := startLocalSyncthing(localBinary, localHome); err != nil {
		return err
	}

	cancelPF, err := startSyncthingPortForward(opts, namespace, pod)
	if err != nil {
		return err
	}
	defer cancelPF()

	localKey, err := readLocalSyncthingAPIKey(localHome)
	if err != nil {
		return err
	}
	remoteKey, err := readRemoteSyncthingAPIKey(ctx, k, namespace, pod)
	if err != nil {
		return err
	}

	localBase := syncthingLocalAPIBase
	remoteBase := syncthingRemoteAPIBase
	if err := waitSyncthingAPI(ctx, localBase, localKey, syncthingAPIReadyTimeout); err != nil {
		return fmt.Errorf("local syncthing not ready: %w", err)
	}
	if err := waitSyncthingAPI(ctx, remoteBase, remoteKey, syncthingAPIReadyTimeout); err != nil {
		return fmt.Errorf("remote syncthing not ready: %w", err)
	}

	localID, err := syncthingDeviceID(ctx, localBase, localKey)
	if err != nil {
		return err
	}
	remoteID, err := syncthingDeviceID(ctx, remoteBase, remoteKey)
	if err != nil {
		return err
	}

	folderTypeLocal, folderTypeRemote := folderTypesForMode(mode)
	folderID := "okdev-" + sessionName

	if err := configureSyncthingPeer(ctx, localBase, localKey, localID, remoteID, syncthingPeerAddrTunnel, folderID, absLocal, folderTypeLocal); err != nil {
		return fmt.Errorf("configure local syncthing: %w", err)
	}
	if err := configureSyncthingPeer(ctx, remoteBase, remoteKey, remoteID, localID, syncthingPeerAddrDynamic, folderID, pair.Remote, folderTypeRemote); err != nil {
		return fmt.Errorf("configure remote syncthing: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Syncthing sync active (%s) for session %s\n", mode, sessionName)
	fmt.Fprintf(cmd.OutOrStdout(), "Local folder: %s\n", absLocal)
	fmt.Fprintf(cmd.OutOrStdout(), "Remote folder: %s\n", pair.Remote)
	fmt.Fprintf(cmd.OutOrStdout(), "Local binary: %s\n", localBinary)
	fmt.Fprintln(cmd.OutOrStdout(), "Press Ctrl+C to stop sync tunnel and local syncthing.")
	progressCtx, stopProgress := context.WithCancel(context.Background())
	defer stopProgress()
	go runSyncthingProgressReporter(progressCtx, cmd.OutOrStdout(), localBase, localKey, remoteBase, remoteKey, folderID, localID, remoteID)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	<-sigCh
	if err := stopLocalSyncthing(localBinary, localHome); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to stop local syncthing: %v\n", err)
	}
	return nil
}

func writeSTIgnore(localPath string, excludes []string) error {
	content, ok := buildSTIgnoreContent(excludes)
	if !ok {
		return nil
	}
	return os.WriteFile(filepath.Join(localPath, ".stignore"), []byte(content), 0o644)
}

func buildSTIgnoreContent(excludes []string) (string, bool) {
	if len(excludes) == 0 {
		return "", false
	}
	var b strings.Builder
	for _, ex := range excludes {
		ex = strings.TrimSpace(ex)
		if ex == "" {
			continue
		}
		b.WriteString(ex)
		if !strings.HasSuffix(ex, "\n") {
			b.WriteString("\n")
		}
	}
	if b.Len() == 0 {
		return "", false
	}
	return b.String(), true
}

func writeRemoteSTIgnoreInPod(ctx context.Context, k interface {
	ExecShInContainer(context.Context, string, string, string, string) ([]byte, error)
}, namespace, pod, remotePath string, excludes []string) error {
	content, ok := buildSTIgnoreContent(excludes)
	if !ok {
		return nil
	}
	ignorePath := strings.TrimRight(remotePath, "/") + "/.stignore"
	if ignorePath == "/.stignore" || strings.TrimSpace(ignorePath) == "" {
		return fmt.Errorf("invalid remote sync path %q for remoteExclude", remotePath)
	}
	cmd := fmt.Sprintf("cat > %s <<'OKDEV_STIGNORE_EOF'\n%sOKDEV_STIGNORE_EOF\n", syncengine.ShellEscape(ignorePath), content)
	_, err := k.ExecShInContainer(ctx, namespace, pod, syncthingContainerName, cmd)
	return err
}

func localSyncthingHome(session string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(home, ".okdev", "syncthing", session)
	if err := os.MkdirAll(p, 0o755); err != nil {
		return "", err
	}
	return p, nil
}

func startLocalSyncthing(binary, home string) error {
	generate := exec.Command(binary, "-home", home, "generate")
	if out, err := generate.CombinedOutput(); err != nil && !strings.Contains(string(out), "already exists") {
		return fmt.Errorf("generate local syncthing config: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	pattern := syncengine.ShellEscape(binary + " -home " + home)
	binaryQ := syncengine.ShellEscape(binary)
	homeQ := syncengine.ShellEscape(home)
	cmd := exec.Command("sh", "-lc", fmt.Sprintf("pkill -f %s >/dev/null 2>&1 || true; nohup %s serve --home %s --no-browser --gui-address=http://%s --no-restart --skip-port-probing >%s 2>&1 &", pattern, binaryQ, homeQ, syncengine.ShellEscape(syncthingLocalGUIAddr), syncengine.ShellEscape(syncthingLocalLogPath)))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("start local syncthing: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func stopLocalSyncthing(binary, home string) error {
	pattern := syncengine.ShellEscape(binary + " -home " + home)
	cmd := exec.Command("sh", "-lc", fmt.Sprintf("pkill -f %s >/dev/null 2>&1 || true", pattern))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("stop local syncthing: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func startSyncthingPortForward(opts *Options, namespace, pod string) (context.CancelFunc, error) {
	return startManagedPortForward(opts, namespace, pod, []string{"18384:8384", "22000:22000"})
}

type syncthingConfigXML struct {
	XMLName xml.Name `xml:"configuration"`
	GUI     struct {
		APIKey string `xml:"apikey"`
	} `xml:"gui"`
}

func readLocalSyncthingAPIKey(home string) (string, error) {
	paths := []string{filepath.Join(home, "config.xml"), filepath.Join(home, "config", "config.xml")}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var cfg syncthingConfigXML
		if err := xml.Unmarshal(b, &cfg); err != nil {
			continue
		}
		if cfg.GUI.APIKey != "" {
			return cfg.GUI.APIKey, nil
		}
	}
	return "", errors.New("unable to read local syncthing API key")
}

func readRemoteSyncthingAPIKey(ctx context.Context, k interface {
	ExecShInContainer(context.Context, string, string, string, string) ([]byte, error)
}, namespace, pod string) (string, error) {
	out, err := k.ExecShInContainer(ctx, namespace, pod, syncthingContainerName, "cat /var/syncthing/config.xml 2>/dev/null || cat /var/syncthing/config/config.xml")
	if err != nil {
		return "", err
	}
	var cfg syncthingConfigXML
	if err := xml.Unmarshal(out, &cfg); err != nil {
		return "", err
	}
	if cfg.GUI.APIKey == "" {
		return "", errors.New("remote syncthing API key is empty")
	}
	return cfg.GUI.APIKey, nil
}

func waitSyncthingAPI(ctx context.Context, base, key string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		_, err := syncthingDeviceID(ctx, base, key)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	return errors.New("API did not become ready")
}

func syncthingDeviceID(ctx context.Context, base, key string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/rest/system/status", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-API-Key", key)
	resp, err := syncthingHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("status %s", resp.Status)
	}
	var payload struct {
		MyID string `json:"myID"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	if payload.MyID == "" {
		return "", errors.New("empty myID from syncthing")
	}
	return payload.MyID, nil
}

func runSyncthingProgressReporter(ctx context.Context, out io.Writer, localBase, localKey, remoteBase, remoteKey, folderID, localID, remoteID string) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	emit := func() bool {
		upPct, upNeed, err := syncthingCompletion(ctx, localBase, localKey, folderID, remoteID)
		if err != nil {
			slog.Debug("syncthing progress read failed", "side", "local", "error", err)
			return false
		}
		downPct, downNeed, err := syncthingCompletion(ctx, remoteBase, remoteKey, folderID, localID)
		if err != nil {
			slog.Debug("syncthing progress read failed", "side", "remote", "error", err)
			return false
		}
		fmt.Fprintf(out, "Syncthing progress: up %.1f%% (need=%dB), down %.1f%% (need=%dB)\n", upPct, upNeed, downPct, downNeed)
		if upNeed == 0 && downNeed == 0 && upPct >= 99.9 && downPct >= 99.9 {
			return true
		}
		return false
	}
	if emit() {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if emit() {
				return
			}
		}
	}
}

func syncthingCompletion(ctx context.Context, base, key, folderID, deviceID string) (float64, int64, error) {
	path := fmt.Sprintf("/rest/db/completion?folder=%s&device=%s", url.QueryEscape(folderID), url.QueryEscape(deviceID))
	body, err := syncthingAPIRequestWithContext(ctx, http.MethodGet, base, key, path, nil, "")
	if err != nil {
		return 0, 0, err
	}
	var payload struct {
		Completion float64 `json:"completion"`
		NeedBytes  int64   `json:"needBytes"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, 0, err
	}
	return payload.Completion, payload.NeedBytes, nil
}

func folderTypesForMode(mode string) (local, remote string) {
	switch mode {
	case "up":
		return "sendonly", "receiveonly"
	case "down":
		return "receiveonly", "sendonly"
	default:
		return "sendreceive", "sendreceive"
	}
}

func configureSyncthingPeer(ctx context.Context, base, key, selfID, peerID, peerAddr, folderID, folderPath, folderType string) error {
	cfg, err := syncthingGetConfig(ctx, base, key)
	if err != nil {
		return err
	}

	devices, err := syncthingObjectArray(cfg, "devices")
	if err != nil {
		return err
	}
	foundDevice := false
	for i, d := range devices {
		m, err := syncthingObjectMap(d, "devices")
		if err != nil {
			return err
		}
		if asString(m["deviceID"]) == peerID {
			m["addresses"] = []any{peerAddr}
			devices[i] = m
			foundDevice = true
			break
		}
	}
	if !foundDevice {
		devices = append(devices, map[string]any{
			"deviceID":  peerID,
			"name":      "okdev-peer",
			"addresses": []any{peerAddr},
		})
	}
	cfg["devices"] = devices

	folders, err := syncthingObjectArray(cfg, "folders")
	if err != nil {
		return err
	}
	foundFolder := false
	for i, f := range folders {
		fm, err := syncthingObjectMap(f, "folders")
		if err != nil {
			return err
		}
		if asString(fm["id"]) == folderID {
			fm["path"] = folderPath
			fm["type"] = folderType
			fm["devices"] = []any{
				map[string]any{"deviceID": selfID},
				map[string]any{"deviceID": peerID},
			}
			folders[i] = fm
			foundFolder = true
			break
		}
	}
	if !foundFolder {
		folders = append(folders, map[string]any{
			"id":    folderID,
			"label": folderID,
			"path":  folderPath,
			"type":  folderType,
			"devices": []any{
				map[string]any{"deviceID": selfID},
				map[string]any{"deviceID": peerID},
			},
		})
	}
	cfg["folders"] = folders

	if err := syncthingSetConfig(ctx, base, key, cfg); err != nil {
		return err
	}
	return syncthingPost(ctx, base, key, "/rest/system/restart", nil)
}

func syncthingGetConfig(ctx context.Context, base, key string) (map[string]any, error) {
	cfg := map[string]any{}
	body, err := syncthingAPIRequestWithContext(ctx, http.MethodGet, base, key, "/rest/config", nil, "")
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(body, &cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func syncthingSetConfig(ctx context.Context, base, key string, cfg map[string]any) error {
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	_, err = syncthingAPIRequestWithContext(ctx, http.MethodPost, base, key, "/rest/config", b, "application/json")
	if err != nil {
		return err
	}
	return nil
}

func syncthingPost(ctx context.Context, base, key, path string, body []byte) error {
	_, err := syncthingAPIRequestWithContext(ctx, http.MethodPost, base, key, path, body, "")
	if err != nil {
		return err
	}
	return nil
}

func syncthingAPIRequestWithContext(ctx context.Context, method, base, key, path string, body []byte, contentType string) ([]byte, error) {
	var reqBody io.Reader
	if len(body) > 0 {
		reqBody = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, reqBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", key)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := syncthingHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, readErr
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %s", resp.Status)
	}
	return respBody, nil
}

func syncthingObjectArray(cfg map[string]any, key string) ([]any, error) {
	raw, ok := cfg[key]
	if !ok || raw == nil {
		return []any{}, nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("syncthing config %q must be an array", key)
	}
	return arr, nil
}

func syncthingObjectMap(v any, field string) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("syncthing config %q entries must be objects", field)
	}
	return m, nil
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
