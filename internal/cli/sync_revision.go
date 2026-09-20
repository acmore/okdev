package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type syncCompletionInfo struct {
	Completion  float64 `json:"completion"`
	NeedBytes   int64   `json:"needBytes"`
	NeedItems   int64   `json:"needItems"`
	NeedDeletes int64   `json:"needDeletes"`
	Sequence    *int64  `json:"sequence"`
	RemoteState string  `json:"remoteState"`
}

func syncthingCompletionInfo(ctx context.Context, base, key, folder, device string) (syncCompletionInfo, error) {
	path := "/rest/db/completion?folder=" + url.QueryEscape(folder) + "&device=" + url.QueryEscape(device)
	body, err := syncthingAPIRequestWithContext(ctx, http.MethodGet, base, key, path, nil, "")
	if err != nil {
		return syncCompletionInfo{}, err
	}
	var info syncCompletionInfo
	err = json.Unmarshal(body, &info)
	return info, err
}

func syncFolderIdle(info syncthingFolderStatusInfo) bool {
	return info.State == "idle" && info.Sequence != nil && info.NeedBytes == 0 && info.NeedFiles == 0 && info.NeedDirectories == 0 && info.NeedSymlinks == 0 && info.NeedDeletes == 0 && info.PullErrors == 0 && info.ReceiveOnlyTotalItems == 0
}

// Each device's sequence is compared with the peer's view of THAT device,
// never with the peer's own sequence (which belongs to a different index).
func syncthingRevisionConvergence(ctx context.Context, localBase, localKey, remoteBase, remoteKey, localID, remoteID, folder string) (int64, string, error) {
	bases := []string{localBase, remoteBase}
	keys := []string{localKey, remoteKey}
	ids := []string{remoteID, localID}
	var before [2]syncthingFolderStatusInfo
	for i := range bases {
		info, err := syncthingFolderStatusInfoForFolder(ctx, bases[i], keys[i], folder)
		if err != nil {
			return 0, "", err
		}
		before[i] = info
		if !syncFolderIdle(info) {
			return info.NeedBytes, fmt.Sprintf("%s: indexing or applying changes (state=%s); current revision not settled", folder, info.State), nil
		}
	}
	var need int64
	for i := range bases {
		info, err := syncthingCompletionInfo(ctx, bases[i], keys[i], folder, ids[i])
		if err != nil {
			return need, "", err
		}
		need += info.NeedBytes
		if info.RemoteState != "valid" {
			return need, fmt.Sprintf("%s: peer %s is %s; delivery unacknowledged", folder, ids[i], info.RemoteState), nil
		}
		if info.Sequence == nil || *info.Sequence < *before[1-i].Sequence {
			return need, fmt.Sprintf("%s: peer index has not acknowledged current revision", folder), nil
		}
		if info.NeedBytes != 0 || info.NeedItems != 0 || info.NeedDeletes != 0 || info.Completion < 100 {
			return need, fmt.Sprintf("%s: waiting for indexed items and deletions", folder), nil
		}
	}
	for i := range bases {
		after, err := syncthingFolderStatusInfoForFolder(ctx, bases[i], keys[i], folder)
		if err != nil {
			return need, "", err
		}
		if !syncFolderIdle(after) || *after.Sequence != *before[i].Sequence {
			return need, fmt.Sprintf("%s: index changed during verification", folder), nil
		}
	}
	return need, "", ctx.Err()
}

func waitSyncthingRevisions(ctx context.Context, localBase, localKey, remoteBase, remoteKey, localID, remoteID string, folders []syncFolder, out io.Writer, receiverChecks ...func(context.Context) error) error {
	// A scan is synchronous. Use the caller's overall deadline instead of the
	// short HTTP timeout; an ambiguous timed-out request cannot prove it ran.
	client := *syncthingHTTPClient
	client.Timeout = 0
	for _, folder := range folders {
		for _, end := range []struct{ base, key string }{{localBase, localKey}, {remoteBase, remoteKey}} {
			fmt.Fprintf(out, "indexing: %s\n", folder.id)
			_, err := syncthingAPIRequestWithClient(ctx, &client, http.MethodPost, end.base, end.key, "/rest/db/scan?folder="+url.QueryEscape(folder.id), nil, "")
			if err != nil {
				return fmt.Errorf("rescan %s did not complete: %w", folder.id, err)
			}
		}
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	last := ""
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("sync did not converge (%s): %w", last, err)
		}
		var reasons []string
		for _, folder := range folders {
			_, reason, err := syncthingRevisionConvergence(ctx, localBase, localKey, remoteBase, remoteKey, localID, remoteID, folder.id)
			if err != nil {
				reason = fmt.Sprintf("%s: %v", folder.id, err)
			}
			if reason != "" {
				reasons = append(reasons, reason)
			}
		}
		if len(reasons) == 0 {
			for _, check := range receiverChecks {
				if err := check(ctx); err != nil {
					reasons = append(reasons, err.Error())
				}
			}
		}
		if len(reasons) == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			fmt.Fprintln(out, "Sync converged for current indexed revisions on the local and target devices; ignored paths are excluded. All discovered intended mesh receivers were verified.")
			return nil
		}
		reason := strings.Join(reasons, "; ")
		if reason != last {
			fmt.Fprintf(out, "waiting: %s\n", reason)
			last = reason
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("sync did not converge (%s): %w", last, ctx.Err())
		case <-ticker.C:
		}
	}
}
