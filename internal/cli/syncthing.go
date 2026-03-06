package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
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

	localBase := "http://127.0.0.1:8385"
	remoteBase := "http://127.0.0.1:18384"
	if err := waitSyncthingAPI(localBase, localKey, 30*time.Second); err != nil {
		return fmt.Errorf("local syncthing not ready: %w", err)
	}
	if err := waitSyncthingAPI(remoteBase, remoteKey, 30*time.Second); err != nil {
		return fmt.Errorf("remote syncthing not ready: %w", err)
	}

	localID, err := syncthingDeviceID(localBase, localKey)
	if err != nil {
		return err
	}
	remoteID, err := syncthingDeviceID(remoteBase, remoteKey)
	if err != nil {
		return err
	}

	folderTypeLocal, folderTypeRemote := folderTypesForMode(mode)
	folderID := "okdev-" + sessionName

	if err := configureSyncthingPeer(localBase, localKey, localID, remoteID, "tcp://127.0.0.1:22000", folderID, absLocal, folderTypeLocal); err != nil {
		return fmt.Errorf("configure local syncthing: %w", err)
	}
	if err := configureSyncthingPeer(remoteBase, remoteKey, remoteID, localID, "dynamic", folderID, pair.Remote, folderTypeRemote); err != nil {
		return fmt.Errorf("configure remote syncthing: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Syncthing sync active (%s) for session %s\n", mode, sessionName)
	fmt.Fprintf(cmd.OutOrStdout(), "Local folder: %s\n", absLocal)
	fmt.Fprintf(cmd.OutOrStdout(), "Remote folder: %s\n", pair.Remote)
	fmt.Fprintf(cmd.OutOrStdout(), "Local binary: %s\n", localBinary)
	fmt.Fprintln(cmd.OutOrStdout(), "Press Ctrl+C to stop sync tunnel and local syncthing.")

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
	if len(excludes) == 0 {
		return nil
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
		return nil
	}
	return os.WriteFile(filepath.Join(localPath, ".stignore"), []byte(b.String()), 0o644)
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
	cmd := exec.Command("sh", "-lc", fmt.Sprintf("pkill -f %s >/dev/null 2>&1 || true; nohup %s -home %s -no-browser -gui-address=127.0.0.1:8385 -no-restart -skip-port-probing -listen=tcp://127.0.0.1:22001 >/tmp/okdev-syncthing-local.log 2>&1 &", pattern, binaryQ, homeQ))
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

func waitSyncthingAPI(base, key string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, err := syncthingDeviceID(base, key)
		if err == nil {
			return nil
		}
		time.Sleep(1 * time.Second)
	}
	return errors.New("API did not become ready")
}

func syncthingDeviceID(base, key string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, base+"/rest/system/status", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-API-Key", key)
	resp, err := http.DefaultClient.Do(req)
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

func configureSyncthingPeer(base, key, selfID, peerID, peerAddr, folderID, folderPath, folderType string) error {
	cfg, err := syncthingGetConfig(base, key)
	if err != nil {
		return err
	}

	devices, _ := cfg["devices"].([]any)
	foundDevice := false
	for i, d := range devices {
		m, ok := d.(map[string]any)
		if !ok {
			continue
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

	folders, _ := cfg["folders"].([]any)
	foundFolder := false
	for i, f := range folders {
		fm, ok := f.(map[string]any)
		if !ok {
			continue
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

	if err := syncthingSetConfig(base, key, cfg); err != nil {
		return err
	}
	return syncthingPost(base, key, "/rest/system/restart", nil)
}

func syncthingGetConfig(base, key string) (map[string]any, error) {
	req, err := http.NewRequest(http.MethodGet, base+"/rest/config", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %s", resp.Status)
	}
	cfg := map[string]any{}
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func syncthingSetConfig(base, key string, cfg map[string]any) error {
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, base+"/rest/config", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("status %s", resp.Status)
	}
	return nil
}

func syncthingPost(base, key, path string, body []byte) error {
	req, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("status %s", resp.Status)
	}
	return nil
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
