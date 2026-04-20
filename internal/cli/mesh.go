package cli

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/acmore/okdev/internal/kube"
	syncengine "github.com/acmore/okdev/internal/sync"
	"github.com/acmore/okdev/internal/workload"
)

const (
	meshReceiverLabelSelector = "okdev.io/mesh-role=receiver"
	meshSetupTimeout          = 2 * time.Minute
	meshSyncPollInterval      = 2 * time.Second
)

type meshReceiverStatus struct {
	Pod       string
	Connected bool
	Synced    bool
	Err       error
}

type meshSummary struct {
	HubPod    string
	Receivers []meshReceiverStatus
}

func meshFolderPath(syncPairs []syncengine.Pair, workspaceMountPath string) string {
	if len(syncPairs) == 1 && strings.TrimSpace(syncPairs[0].Remote) != "" {
		return syncPairs[0].Remote
	}
	return workspaceMountPath
}

// setupMesh configures syncthing mesh sync between the hub (target) pod and
// all receiver pods in the session. It reads the hub's syncthing device ID,
// then configures each receiver's sidecar to peer with the hub. Finally it
// waits for all receivers to complete initial sync.
func setupMesh(ctx context.Context, opts *Options, k *kube.Client, namespace, sessionName string, labels map[string]string, hubPod, folderID, folderPath string, timeout time.Duration, onStatus func(string)) (*meshSummary, error) {
	// 1. Discover receiver pods.
	selector := workload.DiscoveryLabelSelector(labels)
	if selector != "" {
		selector += ","
	}
	selector += meshReceiverLabelSelector

	// Wait for receiver pods to reach Running phase before configuring.
	var receivers []kube.PodSummary
	deadline := time.Now().Add(timeout)
	for {
		pods, err := k.ListPods(ctx, namespace, false, selector)
		if err != nil {
			return nil, fmt.Errorf("discover mesh receiver pods: %w", err)
		}
		receivers = make([]kube.PodSummary, 0, len(pods))
		for _, p := range pods {
			if !p.Deleting && p.Phase == "Running" {
				receivers = append(receivers, p)
			}
		}
		allPods := 0
		for _, p := range pods {
			if !p.Deleting {
				allPods++
			}
		}
		if len(receivers) == allPods && allPods > 0 {
			break
		}
		if allPods == 0 {
			return nil, nil // no receivers at all
		}
		if time.Now().After(deadline) {
			slog.Warn("mesh: timed out waiting for receiver pods to become Running", "running", len(receivers), "total", allPods)
			break
		}
		if onStatus != nil {
			onStatus(fmt.Sprintf("waiting for receiver pods (%d/%d running)", len(receivers), allPods))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	if len(receivers) == 0 {
		return nil, nil
	}

	slog.Debug("mesh: discovered receiver pods", "count", len(receivers))

	// 2. Read hub syncthing device ID via port-forward.
	if onStatus != nil {
		onStatus("reading hub syncthing credentials")
	}
	hubKey, err := readRemoteSyncthingAPIKey(ctx, k, namespace, hubPod)
	if err != nil {
		return nil, fmt.Errorf("read hub syncthing API key: %w", err)
	}
	cancelPF, hubBase, _, err := startSyncthingPortForward(ctx, opts, namespace, hubPod)
	if err != nil {
		return nil, fmt.Errorf("port-forward to hub syncthing: %w", err)
	}
	defer cancelPF()

	if err := waitSyncthingAPI(ctx, hubBase, hubKey, syncthingAPIReadyTimeout); err != nil {
		return nil, fmt.Errorf("hub syncthing API not ready: %w", err)
	}
	hubDeviceID, err := syncthingDeviceID(ctx, hubBase, hubKey)
	if err != nil {
		return nil, fmt.Errorf("read hub syncthing device ID: %w", err)
	}

	// 3. Get hub pod IP for direct pod-to-pod connectivity.
	hubSummary, err := k.GetPodSummary(ctx, namespace, hubPod)
	if err != nil {
		return nil, fmt.Errorf("get hub pod summary: %w", err)
	}
	if hubSummary.PodIP == "" {
		return nil, fmt.Errorf("hub pod %s has no IP", hubPod)
	}
	hubAddr := fmt.Sprintf("tcp://%s:22000", hubSummary.PodIP)
	slog.Debug("mesh: hub info", "deviceID", hubDeviceID, "addr", hubAddr)

	// 4. Read all receiver device IDs and API keys in parallel.
	if onStatus != nil {
		onStatus(fmt.Sprintf("reading credentials for %d receiver(s)", len(receivers)))
	}
	type receiverInfo struct {
		Pod      kube.PodSummary
		APIKey   string
		DeviceID string
		Err      error
	}
	recvInfos := make([]receiverInfo, len(receivers))
	var wg sync.WaitGroup
	for i, recv := range receivers {
		wg.Add(1)
		go func(idx int, pod kube.PodSummary) {
			defer wg.Done()
			info := receiverInfo{Pod: pod}
			key, err := readRemoteSyncthingAPIKey(ctx, k, namespace, pod.Name)
			if err != nil {
				info.Err = fmt.Errorf("read receiver API key: %w", err)
				recvInfos[idx] = info
				return
			}
			info.APIKey = key
			cancelPF, base, _, err := startSyncthingPortForward(ctx, opts, namespace, pod.Name)
			if err != nil {
				info.Err = fmt.Errorf("port-forward to receiver: %w", err)
				recvInfos[idx] = info
				return
			}
			defer cancelPF()
			if err := waitSyncthingAPI(ctx, base, key, syncthingAPIReadyTimeout); err != nil {
				info.Err = fmt.Errorf("receiver syncthing API not ready: %w", err)
				recvInfos[idx] = info
				return
			}
			devID, err := syncthingDeviceID(ctx, base, key)
			if err != nil {
				info.Err = fmt.Errorf("read receiver device ID: %w", err)
				recvInfos[idx] = info
				return
			}
			info.DeviceID = devID
			recvInfos[idx] = info
		}(i, recv)
	}
	wg.Wait()

	// 5. Configure hub to know about ALL receivers BEFORE receivers connect.
	if onStatus != nil {
		onStatus("configuring hub with receiver device IDs")
	}
	hubPeers := make(map[string]string)
	for i, info := range recvInfos {
		if info.Err != nil {
			continue
		}
		if info.Pod.PodIP == "" {
			recvInfos[i].Err = fmt.Errorf("receiver pod IP unavailable for hub config")
			slog.Warn("mesh: receiver pod IP unavailable for hub config", "pod", info.Pod.Name)
			continue
		}
		hubPeers[info.DeviceID] = fmt.Sprintf("tcp://%s:22000", info.Pod.PodIP)
	}
	if len(hubPeers) > 0 {
		if err := configureSyncthingMeshHub(ctx, hubBase, hubKey, hubDeviceID, hubPeers, folderID, folderPath); err != nil {
			return nil, fmt.Errorf("configure hub syncthing mesh: %w", err)
		}
	}

	// 6. Configure each receiver sidecar to peer with hub, then wait for sync.
	if onStatus != nil {
		onStatus(fmt.Sprintf("configuring %d receiver sidecar(s)", len(receivers)))
	}
	results := make([]meshReceiverStatus, len(receivers))
	for i, info := range recvInfos {
		wg.Add(1)
		go func(idx int, ri receiverInfo) {
			defer wg.Done()
			if ri.Err != nil {
				results[idx] = meshReceiverStatus{Pod: ri.Pod.Name, Err: ri.Err}
				return
			}
			results[idx] = configureAndWaitMeshReceiver(ctx, opts, k, namespace, ri.Pod, ri.APIKey, ri.DeviceID, hubBase, hubKey, hubDeviceID, hubAddr, folderID, folderPath, timeout)
		}(i, info)
	}
	wg.Wait()

	summary := &meshSummary{
		HubPod:    hubPod,
		Receivers: results,
	}
	return summary, nil
}

func configureSyncthingMeshHub(ctx context.Context, base, key, hubDeviceID string, receiverDeviceAddrs map[string]string, folderID, folderPath string) error {
	cfg, err := syncthingGetConfig(ctx, base, key)
	if err != nil {
		return err
	}
	applyManagedSyncthingGlobalDefaults(cfg, false)

	devices, err := syncthingObjectArray(cfg, "devices")
	if err != nil {
		return err
	}
	deviceIndex := make(map[string]int, len(devices))
	for i, d := range devices {
		m, err := syncthingObjectMap(d, "devices")
		if err != nil {
			return err
		}
		if id := asString(m["deviceID"]); id != "" {
			deviceIndex[id] = i
		}
	}

	receiverIDs := make([]string, 0, len(receiverDeviceAddrs))
	for id := range receiverDeviceAddrs {
		receiverIDs = append(receiverIDs, id)
	}
	sort.Strings(receiverIDs)
	for _, id := range receiverIDs {
		addresses := syncthingDeviceAddresses(receiverDeviceAddrs[id])
		if idx, ok := deviceIndex[id]; ok {
			m, err := syncthingObjectMap(devices[idx], "devices")
			if err != nil {
				return err
			}
			m["addresses"] = addresses
			m["compression"] = "metadata"
			applyManagedSyncthingDeviceDefaults(m)
			devices[idx] = m
			continue
		}
		device := map[string]any{
			"deviceID":    id,
			"name":        "okdev-mesh-receiver",
			"addresses":   addresses,
			"compression": "metadata",
		}
		applyManagedSyncthingDeviceDefaults(device)
		devices = append(devices, device)
	}
	cfg["devices"] = devices

	folders, err := syncthingObjectArray(cfg, "folders")
	if err != nil {
		return err
	}
	folderDevices := make([]any, 0, len(receiverIDs)+1)
	folderDevices = append(folderDevices, map[string]any{"deviceID": hubDeviceID})
	for _, id := range receiverIDs {
		folderDevices = append(folderDevices, map[string]any{"deviceID": id})
	}

	filteredFolders := make([]any, 0, len(folders))
	foundFolder := false
	for _, f := range folders {
		fm, err := syncthingObjectMap(f, "folders")
		if err != nil {
			return err
		}
		if asString(fm["id"]) == "default" {
			continue
		}
		if asString(fm["id"]) == folderID {
			fm["path"] = folderPath
			fm["type"] = "sendreceive"
			fm["markerName"] = "."
			mergedDevices, err := syncthingMergeMeshHubFolderDevices(fm["devices"], devices, hubDeviceID, receiverIDs)
			if err != nil {
				return err
			}
			fm["devices"] = mergedDevices
			applyManagedSyncthingFolderDefaults(fm, 60, 1, false)
			filteredFolders = append(filteredFolders, fm)
			foundFolder = true
			continue
		}
		filteredFolders = append(filteredFolders, fm)
	}
	if !foundFolder {
		folder := map[string]any{
			"id":         folderID,
			"label":      folderID,
			"path":       folderPath,
			"type":       "sendreceive",
			"markerName": ".",
			"devices":    folderDevices,
		}
		applyManagedSyncthingFolderDefaults(folder, 60, 1, false)
		filteredFolders = append(filteredFolders, folder)
	}
	cfg["folders"] = filteredFolders

	return syncthingSetConfig(ctx, base, key, cfg)
}

func syncthingMergeMeshHubFolderDevices(existingFolderDevices, devices any, hubDeviceID string, receiverIDs []string) ([]any, error) {
	deviceEntries, err := syncthingObjectArray(map[string]any{"devices": devices}, "devices")
	if err != nil {
		return nil, err
	}
	deviceNames := make(map[string]string, len(deviceEntries))
	for _, d := range deviceEntries {
		m, err := syncthingObjectMap(d, "devices")
		if err != nil {
			return nil, err
		}
		deviceNames[asString(m["deviceID"])] = asString(m["name"])
	}

	merged := make([]any, 0, 1+len(receiverIDs)+1)
	merged = append(merged, map[string]any{"deviceID": hubDeviceID})
	receiverSet := make(map[string]struct{}, len(receiverIDs))
	for _, id := range receiverIDs {
		receiverSet[id] = struct{}{}
	}

	folderEntries, ok := existingFolderDevices.([]any)
	if ok {
		seen := map[string]struct{}{hubDeviceID: {}}
		for _, d := range folderEntries {
			m, err := syncthingObjectMap(d, "folder devices")
			if err != nil {
				return nil, err
			}
			id := asString(m["deviceID"])
			if id == "" {
				continue
			}
			if _, exists := seen[id]; exists {
				continue
			}
			if _, isReceiver := receiverSet[id]; isReceiver {
				continue
			}
			if deviceNames[id] == "okdev-mesh-receiver" {
				continue
			}
			seen[id] = struct{}{}
			merged = append(merged, map[string]any{"deviceID": id})
		}
		for _, id := range receiverIDs {
			if _, exists := seen[id]; exists {
				continue
			}
			seen[id] = struct{}{}
			merged = append(merged, map[string]any{"deviceID": id})
		}
		return merged, nil
	}

	for _, id := range receiverIDs {
		merged = append(merged, map[string]any{"deviceID": id})
	}
	return merged, nil
}

// configureAndWaitMeshReceiver configures a single receiver pod's syncthing
// to peer with the hub and waits for initial sync to complete.
func configureAndWaitMeshReceiver(ctx context.Context, opts *Options, k *kube.Client, namespace string, pod kube.PodSummary, recvKey, recvDeviceID, hubBase, hubKey, hubDeviceID, hubAddr, folderID, folderPath string, timeout time.Duration) meshReceiverStatus {
	status := meshReceiverStatus{Pod: pod.Name}

	// Ensure the receiver sidecar has syncthing running.
	if _, err := execInSyncthingContainer(ctx, k, namespace, pod.Name, "command -v syncthing >/dev/null 2>&1"); err != nil {
		status.Err = fmt.Errorf("syncthing not available in sidecar: %w", err)
		return status
	}

	cancelPF, recvBase, _, err := startSyncthingPortForward(ctx, opts, namespace, pod.Name)
	if err != nil {
		status.Err = fmt.Errorf("port-forward to receiver: %w", err)
		return status
	}
	defer cancelPF()

	if err := waitSyncthingAPI(ctx, recvBase, recvKey, syncthingAPIReadyTimeout); err != nil {
		status.Err = fmt.Errorf("receiver syncthing API not ready: %w", err)
		return status
	}

	// Ensure workspace dir exists on receiver.
	if _, err := execInSyncthingContainer(ctx, k, namespace, pod.Name, fmt.Sprintf("mkdir -p %s", folderPath)); err != nil {
		slog.Debug("mesh: mkdir workspace on receiver", "pod", pod.Name, "error", err)
	}

	// Configure receiver syncthing to peer with hub as receiveonly.
	if err := configureSyncthingPeer(ctx, recvBase, recvKey,
		recvDeviceID, hubDeviceID,
		hubAddr,
		folderID, folderPath,
		"receiveonly",
		60, 1, // rescan/watcher intervals
		false, false, // ignoreDelete, relaysEnabled
		false, // compression
	); err != nil {
		status.Err = fmt.Errorf("configure receiver syncthing: %w", err)
		return status
	}

	slog.Debug("mesh: receiver configured", "pod", pod.Name, "deviceID", recvDeviceID)
	status.Connected = true

	// Wait for receiver to sync (needBytes == 0).
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(meshSyncPollInterval)
	defer ticker.Stop()
	for {
		receiverConnected, recvConnErr := syncthingPeerConnected(ctx, recvBase, recvKey, hubDeviceID)
		hubConnected, hubConnErr := syncthingPeerConnected(ctx, hubBase, hubKey, recvDeviceID)
		if recvConnErr != nil {
			slog.Debug("mesh: receiver connection poll error", "pod", pod.Name, "error", recvConnErr)
		}
		if hubConnErr != nil {
			slog.Debug("mesh: hub connection poll error", "pod", pod.Name, "error", hubConnErr)
		}
		if receiverConnected && hubConnected {
			status.Connected = true
		}

		_, needBytes, pollErr := syncthingCompletion(ctx, recvBase, recvKey, folderID, hubDeviceID)
		if pollErr == nil && needBytes == 0 && status.Connected {
			status.Synced = true
			slog.Debug("mesh: receiver synced", "pod", pod.Name)
			return status
		}
		if pollErr != nil {
			slog.Debug("mesh: receiver sync poll error", "pod", pod.Name, "error", pollErr)
		}
		if time.Now().After(deadline) {
			if !status.Connected {
				status.Err = fmt.Errorf("mesh sync timed out waiting for hub/receiver connection")
			} else if pollErr != nil {
				status.Err = fmt.Errorf("mesh sync timed out: %w", pollErr)
			} else {
				status.Err = fmt.Errorf("mesh sync timed out, %d bytes remaining", needBytes)
			}
			return status
		}
		select {
		case <-ctx.Done():
			status.Err = ctx.Err()
			return status
		case <-ticker.C:
		}
	}
}

// meshReceiverHealth describes the live syncthing health of a single receiver.
type meshReceiverHealth struct {
	Pod       string `json:"pod"`
	Connected bool   `json:"connected"`
	InSync    bool   `json:"inSync"`
	NeedBytes int64  `json:"needBytes,omitempty"`
	Err       string `json:"error,omitempty"`
}

// meshHealthSummary is the result of probing all mesh receivers.
type meshHealthSummary struct {
	HubPod    string               `json:"hubPod"`
	FolderID  string               `json:"folderID"`
	Receivers []meshReceiverHealth `json:"receivers"`
}

// checkMeshHealth probes each mesh receiver's syncthing API to determine
// connection and sync status. It returns per-receiver health without
// modifying any configuration.
func checkMeshHealth(ctx context.Context, opts *Options, k *kube.Client, namespace, sessionName string, labels map[string]string, hubPod, folderID string) (*meshHealthSummary, error) {
	selector := workload.DiscoveryLabelSelector(labels)
	if selector != "" {
		selector += ","
	}
	selector += meshReceiverLabelSelector
	pods, err := k.ListPods(ctx, namespace, false, selector)
	if err != nil {
		return nil, fmt.Errorf("list mesh receiver pods: %w", err)
	}
	var receivers []kube.PodSummary
	for _, p := range pods {
		if !p.Deleting && p.Phase == "Running" {
			receivers = append(receivers, p)
		}
	}
	if len(receivers) == 0 {
		return nil, nil
	}

	hubKey, err := readRemoteSyncthingAPIKey(ctx, k, namespace, hubPod)
	if err != nil {
		return nil, fmt.Errorf("read hub API key: %w", err)
	}
	cancelPF, hubBase, _, err := startSyncthingPortForward(ctx, opts, namespace, hubPod)
	if err != nil {
		return nil, fmt.Errorf("port-forward to hub: %w", err)
	}
	defer cancelPF()
	if err := waitSyncthingAPI(ctx, hubBase, hubKey, syncthingAPIReadyTimeout); err != nil {
		return nil, fmt.Errorf("hub syncthing API not ready: %w", err)
	}

	results := make([]meshReceiverHealth, len(receivers))
	var wg sync.WaitGroup
	for i, recv := range receivers {
		wg.Add(1)
		go func(idx int, pod kube.PodSummary) {
			defer wg.Done()
			results[idx] = probeMeshReceiver(ctx, opts, k, namespace, pod, hubBase, hubKey, folderID)
		}(i, recv)
	}
	wg.Wait()

	return &meshHealthSummary{HubPod: hubPod, FolderID: folderID, Receivers: results}, nil
}

func probeMeshReceiver(ctx context.Context, opts *Options, k *kube.Client, namespace string, pod kube.PodSummary, hubBase, hubKey, folderID string) meshReceiverHealth {
	h := meshReceiverHealth{Pod: pod.Name}

	recvKey, err := readRemoteSyncthingAPIKey(ctx, k, namespace, pod.Name)
	if err != nil {
		h.Err = fmt.Sprintf("read API key: %v", err)
		return h
	}
	cancelPF, recvBase, _, err := startSyncthingPortForward(ctx, opts, namespace, pod.Name)
	if err != nil {
		h.Err = fmt.Sprintf("port-forward: %v", err)
		return h
	}
	defer cancelPF()
	if err := waitSyncthingAPI(ctx, recvBase, recvKey, 5*time.Second); err != nil {
		h.Err = fmt.Sprintf("API not ready: %v", err)
		return h
	}

	recvDeviceID, err := syncthingDeviceID(ctx, recvBase, recvKey)
	if err != nil {
		h.Err = fmt.Sprintf("read device ID: %v", err)
		return h
	}

	hubDeviceID, err := syncthingDeviceID(ctx, hubBase, hubKey)
	if err != nil {
		h.Err = fmt.Sprintf("read hub device ID: %v", err)
		return h
	}

	recvConn, _ := syncthingPeerConnected(ctx, recvBase, recvKey, hubDeviceID)
	hubConn, _ := syncthingPeerConnected(ctx, hubBase, hubKey, recvDeviceID)
	h.Connected = recvConn && hubConn

	_, needBytes, pollErr := syncthingCompletion(ctx, recvBase, recvKey, folderID, hubDeviceID)
	if pollErr == nil {
		h.NeedBytes = needBytes
		h.InSync = needBytes == 0 && h.Connected
	} else {
		h.Err = fmt.Sprintf("completion poll: %v", pollErr)
	}
	return h
}

// brokenMeshReceiverPods returns the pod names of receivers that are not
// connected or not in sync.
func brokenMeshReceiverPods(summary *meshHealthSummary) []string {
	if summary == nil {
		return nil
	}
	var broken []string
	for _, r := range summary.Receivers {
		if r.Err != "" || !r.Connected || !r.InSync {
			broken = append(broken, r.Pod)
		}
	}
	return broken
}

// repairMeshReceivers re-runs the full mesh setup which reconfigures all
// receivers. setupMesh is idempotent — healthy receivers reconverge quickly
// while broken ones get reconfigured.
func repairMeshReceivers(ctx context.Context, opts *Options, k *kube.Client, namespace, sessionName string, labels map[string]string, hubPod, folderID, folderPath string, onStatus func(string)) (*meshSummary, error) {
	return setupMesh(ctx, opts, k, namespace, sessionName, labels, hubPod, folderID, folderPath, meshSetupTimeout, onStatus)
}

// meshReceiverCount returns the number of receiver pods discovered in a session.
func meshReceiverCount(ctx context.Context, k *kube.Client, namespace string, labels map[string]string) (int, error) {
	selector := workload.DiscoveryLabelSelector(labels)
	if selector != "" {
		selector += ","
	}
	selector += meshReceiverLabelSelector
	pods, err := k.ListPods(ctx, namespace, false, selector)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, p := range pods {
		if !p.Deleting {
			count++
		}
	}
	return count, nil
}

// formatMeshSummary returns a human-readable summary for the mesh setup step.
func formatMeshSummary(summary *meshSummary) string {
	if summary == nil {
		return "no receivers"
	}
	connected := 0
	synced := 0
	failed := 0
	for _, r := range summary.Receivers {
		if r.Err != nil {
			failed++
			continue
		}
		if r.Connected {
			connected++
		}
		if r.Synced {
			synced++
		}
	}
	total := len(summary.Receivers)
	if failed > 0 {
		return fmt.Sprintf("%d/%d receiver(s) synced, %d failed", synced, total, failed)
	}
	return fmt.Sprintf("%d receiver(s) connected and synced", synced)
}

// meshStatusLines returns lines for the status --details mesh section.
func meshStatusLines(summary *meshSummary) []string {
	if summary == nil {
		return nil
	}
	lines := []string{
		"topology: hub-and-spoke",
		fmt.Sprintf("hub: %s", summary.HubPod),
	}
	connected := 0
	for _, r := range summary.Receivers {
		if r.Err == nil && r.Connected {
			connected++
		}
	}
	lines = append(lines, fmt.Sprintf("receivers: %d/%d connected", connected, len(summary.Receivers)))
	for _, r := range summary.Receivers {
		state := "synced"
		if r.Err != nil {
			state = "error: " + r.Err.Error()
		} else if !r.Synced {
			state = "pending"
		}
		lines = append(lines, fmt.Sprintf("  %s: %s", r.Pod, state))
	}
	return lines
}
