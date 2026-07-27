package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	syncengine "github.com/acmore/okdev/internal/sync"
)

// syncStatusDefaultTopN is how many pending files are named. Enough to see the
// shape of the transfer; the share column tells the reader when one entry is
// the whole story.
const syncStatusDefaultTopN = 5

// syncPendingFile is one entry of the actual pending set — what syncthing
// still has to move, as opposed to what happens to be large on disk.
type syncPendingFile struct {
	Name      string  `json:"name"`
	Size      int64   `json:"size"`
	Share     float64 `json:"share"`
	Direction string  `json:"direction"`
}

type syncFolderStatus struct {
	Folder    string            `json:"folder"`
	Local     string            `json:"local"`
	Remote    string            `json:"remote"`
	Direction string            `json:"direction"`
	State     string            `json:"state"`
	NeedBytes int64             `json:"needBytes"`
	NeedFiles int64             `json:"needFiles"`
	Pending   []syncPendingFile `json:"pending,omitempty"`
	Excludes  []string          `json:"excludes,omitempty"`
	// ExcludeSource distinguishes patterns read from a .stignore file from
	// okdev's built-in defaults, which apply when no file exists. Reporting
	// defaults as if they came from a file would send the reader editing a
	// file that is not there.
	ExcludeSource string `json:"excludeSource,omitempty"`
	Error         string `json:"error,omitempty"`
}

type syncStatusReport struct {
	Session   string             `json:"session"`
	Converged bool               `json:"converged"`
	NeedBytes int64              `json:"needBytes"`
	NeedFiles int64              `json:"needFiles"`
	Folders   []syncFolderStatus `json:"folders"`
}

func newSyncStatusCmd(opts *Options) *cobra.Command {
	var topN int

	cmd := &cobra.Command{
		Use:   "status [session]",
		Short: "Show what sync still has to transfer, and which files",
		Long: `Report the pending transfer for every sync mapping: bytes and file count
still needed in each direction, the largest actually-pending files with their
share of the transfer, and the .stignore patterns currently in force.

Unlike the size heuristic printed during "okdev up", this is syncthing's real
pending set — a file that is already synced never appears, and a file pending
from the pod does. Use it when sync is slow or stuck to find out what is
actually moving; "okdev sync wait" blocks until it is empty.`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: sessionCompletionFunc(opts),
		RunE: func(cmd *cobra.Command, args []string) error {
			applySessionArg(opts, args)
			cc, err := resolveCommandContext(opts, resolveSessionName)
			if err != nil {
				return err
			}
			if err := ensureExistingSessionOwnership(cc.opts, cc.kube, cc.namespace, cc.sessionName); err != nil {
				return err
			}
			if len(cc.cfg.Spec.Sync.Paths) == 0 {
				return fmt.Errorf("session %s has no sync mappings", cc.sessionName)
			}
			pairs, err := syncengine.ParsePairs(cc.cfg.Spec.Sync.Paths, cc.cfg.EffectiveWorkspaceMountPath(cc.cfgPath))
			if err != nil {
				return err
			}
			target, err := resolveTargetRef(cmd.Context(), cc.opts, cc.cfg, cc.namespace, cc.sessionName, cc.kube)
			if err != nil {
				return err
			}
			report, err := gatherSyncStatus(cmd.Context(), cc, target.PodName, pairs, topN)
			if err != nil {
				return err
			}
			if opts.Output == "json" {
				return outputJSON(cmd.OutOrStdout(), report)
			}
			printSyncStatus(cmd.OutOrStdout(), report)
			return nil
		},
	}

	cmd.Flags().IntVar(&topN, "top", syncStatusDefaultTopN, "How many pending files to name per folder")
	return cmd
}

// gatherSyncStatus queries both syncthing instances for each folder's pending
// set. Both ends are asked because the direction of the pending work is part
// of the answer: bytes needed locally are coming from the pod, bytes needed
// remotely are going to it.
func gatherSyncStatus(ctx context.Context, cc *commandContext, pod string, pairs []syncengine.Pair, topN int) (syncStatusReport, error) {
	if topN <= 0 {
		topN = syncStatusDefaultTopN
	}
	folders, err := resolveSyncFolders(cc.sessionName, "", pairs)
	if err != nil {
		return syncStatusReport{}, err
	}
	localHome, err := localSyncthingStatusHome(cc.sessionName)
	if err != nil {
		return syncStatusReport{}, fmt.Errorf("resolve local syncthing home: %w", err)
	}
	localBase, localKey, err := readLocalSyncthingEndpoint(localHome)
	if err != nil {
		return syncStatusReport{}, fmt.Errorf("read local syncthing endpoint: %w", err)
	}
	remoteKey, err := readRemoteSyncthingAPIKey(ctx, cc.kube, cc.namespace, pod)
	if err != nil {
		return syncStatusReport{}, fmt.Errorf("read remote syncthing API key: %w", err)
	}
	cancelPF, remoteBase, _, err := startSyncthingPortForward(ctx, cc.opts, cc.namespace, pod)
	if err != nil {
		return syncStatusReport{}, fmt.Errorf("port-forward to syncthing sidecar: %w", err)
	}
	defer cancelPF()
	if err := waitSyncthingAPI(ctx, remoteBase, remoteKey, syncthingAPIReadyTimeout); err != nil {
		return syncStatusReport{}, fmt.Errorf("remote syncthing not ready: %w", err)
	}
	if err := waitSyncthingAPI(ctx, localBase, localKey, syncthingAPIReadyTimeout); err != nil {
		return syncStatusReport{}, fmt.Errorf("local syncthing not ready: %w", err)
	}

	report := syncStatusReport{Session: cc.sessionName, Converged: true}
	for _, folder := range folders {
		entry := syncFolderStatus{
			Folder:    folder.id,
			Local:     folder.absLocal,
			Remote:    folder.remote,
			Direction: folder.mode,
		}
		var problems []string
		for _, end := range []struct {
			base, key, direction string
		}{
			{localBase, localKey, "remote->local"},
			{remoteBase, remoteKey, "local->remote"},
		} {
			info, statusErr := syncthingFolderStatusInfoForFolder(ctx, end.base, end.key, folder.id)
			if statusErr != nil {
				problems = append(problems, fmt.Sprintf("%s: %v", end.direction, statusErr))
				continue
			}
			if entry.State == "" || info.NeedBytes > 0 {
				entry.State = info.State
			}
			entry.NeedBytes += info.NeedBytes
			entry.NeedFiles += info.NeedFiles
			if info.NeedBytes == 0 && info.NeedFiles == 0 {
				continue
			}
			pending, needErr := syncthingFolderNeed(ctx, end.base, end.key, folder.id, topN)
			if needErr != nil {
				problems = append(problems, fmt.Sprintf("%s: %v", end.direction, needErr))
				continue
			}
			for i := range pending {
				pending[i].Direction = end.direction
			}
			entry.Pending = append(entry.Pending, pending...)
		}
		entry.Error = strings.Join(problems, "; ")
		entry.Excludes, entry.ExcludeSource = loadSTIgnoreDisplayPatterns(folder.absLocal)
		entry.Pending = topPendingFiles(entry.Pending, entry.NeedBytes, topN)
		if entry.NeedBytes > 0 || entry.NeedFiles > 0 || entry.Error != "" {
			report.Converged = false
		}
		report.NeedBytes += entry.NeedBytes
		report.NeedFiles += entry.NeedFiles
		report.Folders = append(report.Folders, entry)
	}
	return report, nil
}

// syncthingFolderNeed reads the actual pending set from /rest/db/need. The
// endpoint returns three buckets — in-progress, queued, and the rest — and all
// three are pending work, so all three are read.
func syncthingFolderNeed(ctx context.Context, base, key, folderID string, limit int) ([]syncPendingFile, error) {
	if limit <= 0 {
		limit = syncStatusDefaultTopN
	}
	// perpage is deliberately larger than the display limit: the API returns
	// its own ordering, and picking the largest entries out of a one-page
	// sample beats reporting whichever happened to be first.
	perPage := limit * 20
	if perPage < 100 {
		perPage = 100
	}
	path := fmt.Sprintf("/rest/db/need?folder=%s&page=1&perpage=%d", url.QueryEscape(folderID), perPage)
	body, err := syncthingAPIRequestWithContext(ctx, http.MethodGet, base, key, path, nil, "")
	if err != nil {
		return nil, err
	}
	var payload struct {
		Progress []syncthingNeedEntry `json:"progress"`
		Queued   []syncthingNeedEntry `json:"queued"`
		Rest     []syncthingNeedEntry `json:"rest"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	out := make([]syncPendingFile, 0, len(payload.Progress)+len(payload.Queued)+len(payload.Rest))
	for _, bucket := range [][]syncthingNeedEntry{payload.Progress, payload.Queued, payload.Rest} {
		for _, entry := range bucket {
			if entry.Deleted || strings.TrimSpace(entry.Name) == "" || !needEntryIsFile(entry.Type) {
				continue
			}
			out = append(out, syncPendingFile{Name: entry.Name, Size: entry.Size})
		}
	}
	return out, nil
}

type syncthingNeedEntry struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Deleted bool   `json:"deleted"`
	// Type is decoded raw because syncthing has encoded it both as an enum
	// string ("FILE_INFO_TYPE_DIRECTORY") and as an integer across versions.
	Type json.RawMessage `json:"type"`
}

// needEntryIsFile filters out directory and symlink entries. They are pending
// items too, but listing a directory among "the largest pending files" is
// noise — a directory's reported size is metadata, not transfer volume.
func needEntryIsFile(raw json.RawMessage) bool {
	value := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if value == "" {
		return true // no type reported: assume a file rather than drop it
	}
	if upper := strings.ToUpper(value); strings.Contains(upper, "DIRECTORY") || strings.Contains(upper, "SYMLINK") {
		return false
	}
	// Integer encoding: 0 is a file, everything else is a directory or symlink.
	if value[0] >= '0' && value[0] <= '9' {
		return value == "0"
	}
	return true
}

// topPendingFiles keeps the largest entries and attributes each a share of the
// transfer. The share is the actionable number: "this one file is 92% of what
// is left" is a one-line .stignore fix, where "something is big" is not.
//
// The denominator is the larger of the reported pending bytes and the sizes we
// can see, because the two are measured differently: needBytes shrinks as
// blocks arrive while a need entry keeps its whole file size, so dividing by
// needBytes alone reports a half-transferred file at over 100%. Taking the
// maximum keeps every share inside 0-100% whether the transfer is mid-flight
// or the sampled page is only part of a very long need list.
func topPendingFiles(files []syncPendingFile, needBytes int64, limit int) []syncPendingFile {
	if len(files) == 0 {
		return nil
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].Size > files[j].Size })
	var sampled int64
	for _, file := range files {
		sampled += file.Size
	}
	if limit > 0 && len(files) > limit {
		files = files[:limit]
	}
	denominator := needBytes
	if sampled > denominator {
		denominator = sampled
	}
	if denominator > 0 {
		for i := range files {
			share := float64(files[i].Size) / float64(denominator)
			if share > 1 {
				share = 1
			}
			files[i].Share = share
		}
	}
	return files
}

// loadSTIgnoreDisplayPatterns lists the exclude patterns in force and where
// they come from. Reported alongside the pending set because the two answer one
// question together: what is moving, and what was already excluded from moving.
func loadSTIgnoreDisplayPatterns(root string) ([]string, string) {
	source := ".stignore"
	if _, err := os.Stat(filepath.Join(root, ".stignore")); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, ""
		}
		source = "okdev defaults (no .stignore file)"
	}
	patterns, err := loadSTIgnorePatterns(root)
	if err != nil {
		return nil, ""
	}
	out := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" || strings.HasPrefix(pattern, "//") {
			continue
		}
		out = append(out, pattern)
	}
	if len(out) == 0 {
		return nil, ""
	}
	return out, source
}

func printSyncStatus(w io.Writer, report syncStatusReport) {
	if report.Converged {
		fmt.Fprintf(w, "Sync converged (%d folder(s)); nothing pending.\n", len(report.Folders))
	} else {
		fmt.Fprintf(w, "Pending: %s across %d file(s).\n", humanSizeIEC(report.NeedBytes), report.NeedFiles)
	}
	for _, folder := range report.Folders {
		fmt.Fprintf(w, "\n%s  %s <-> %s  (direction: %s, state: %s)\n",
			folder.Folder, folder.Local, folder.Remote, folder.Direction, emptyDash(folder.State))
		fmt.Fprintf(w, "- pending: %s across %d file(s)\n", humanSizeIEC(folder.NeedBytes), folder.NeedFiles)
		for _, file := range folder.Pending {
			line := fmt.Sprintf("  %8s  %s  %s", humanSizeIEC(file.Size), file.Name, file.Direction)
			if file.Share > 0 {
				line = fmt.Sprintf("  %8s  %5.1f%%  %s  %s", humanSizeIEC(file.Size), file.Share*100, file.Name, file.Direction)
			}
			fmt.Fprintln(w, line)
		}
		if len(folder.Excludes) > 0 {
			fmt.Fprintf(w, "- excludes in force (%d, from %s): %s\n", len(folder.Excludes), folder.ExcludeSource, strings.Join(folder.Excludes, ", "))
		} else {
			fmt.Fprintln(w, "- excludes in force: none")
		}
		if folder.Error != "" {
			fmt.Fprintf(w, "- warning: %s\n", folder.Error)
		}
	}
}
