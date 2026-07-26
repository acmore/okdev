package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTopPendingFilesRanksAndAttributesShare(t *testing.T) {
	files := []syncPendingFile{
		{Name: "small.txt", Size: 100},
		{Name: "dump.pt", Size: 9_000},
		{Name: "mid.bin", Size: 900},
	}
	got := topPendingFiles(files, 10_000, 2)
	if len(got) != 2 || got[0].Name != "dump.pt" || got[1].Name != "mid.bin" {
		t.Fatalf("expected the two largest in size order, got %+v", got)
	}
	// The share is the actionable number: "this one file is 90% of what is
	// left" is a one-line .stignore fix; "something is big" is not.
	if got[0].Share < 0.89 || got[0].Share > 0.91 {
		t.Fatalf("expected ~0.9 share for the dominant file, got %v", got[0].Share)
	}
}

func TestTopPendingFilesNeverReportsMoreThanTheWholeTransfer(t *testing.T) {
	// Mid-transfer, needBytes has already shrunk below the file's full size —
	// dividing by it alone reported a half-moved 192 MiB file as 107% of the
	// transfer, which is the one number this feature exists to get right.
	got := topPendingFiles([]syncPendingFile{{Name: "blob.bin", Size: 192}}, 179, 5)
	if got[0].Share != 1 {
		t.Fatalf("expected a whole-transfer share of 1, got %v", got[0].Share)
	}
	// And a page that samples only part of a long need list must not inflate
	// the shares of what it did see.
	partial := topPendingFiles([]syncPendingFile{{Name: "a", Size: 60}, {Name: "b", Size: 40}}, 1000, 5)
	if partial[0].Share < 0.05 || partial[0].Share > 0.07 {
		t.Fatalf("expected the share against the reported pending total, got %v", partial[0].Share)
	}
}

func TestNeedEntryIsFile(t *testing.T) {
	tests := map[string]bool{
		`"FILE_INFO_TYPE_FILE"`:      true,
		`"FILE_INFO_TYPE_DIRECTORY"`: false,
		`"FILE_INFO_TYPE_SYMLINK"`:   false,
		`0`:                          true,
		`1`:                          false,
		`2`:                          false,
		``:                           true,
	}
	for raw, want := range tests {
		if got := needEntryIsFile([]byte(raw)); got != want {
			t.Fatalf("needEntryIsFile(%s) = %v, want %v", raw, got, want)
		}
	}
}

func TestTopPendingFilesWithoutNeedBytes(t *testing.T) {
	// No reported total, but the sampled entries are a total of their own —
	// one pending file is then all of the visible transfer.
	got := topPendingFiles([]syncPendingFile{{Name: "a", Size: 5}}, 0, 5)
	if len(got) != 1 || got[0].Share != 1 {
		t.Fatalf("expected the share to fall back to the sampled total, got %+v", got)
	}
	if topPendingFiles(nil, 100, 5) != nil {
		t.Fatal("no pending files must produce no entries")
	}
}

func TestLoadSTIgnoreDisplayPatternsDropsNoise(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".stignore"), []byte("// managed block\n\n.venv\noutputs/**\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, source := loadSTIgnoreDisplayPatterns(root)
	if len(got) != 2 || got[0] != ".venv" || got[1] != "outputs/**" {
		t.Fatalf("expected the effective patterns without comments or blanks, got %+v", got)
	}
	if source != ".stignore" {
		t.Fatalf("expected the file as the source, got %q", source)
	}
	// No .stignore does not mean nothing is excluded — okdev's defaults apply,
	// and saying they came from a file would send the reader editing one that
	// does not exist.
	defaults, defaultSource := loadSTIgnoreDisplayPatterns(t.TempDir())
	if len(defaults) == 0 || !strings.Contains(defaultSource, "okdev defaults") {
		t.Fatalf("expected okdev defaults to be reported as such, got %+v from %q", defaults, defaultSource)
	}
}

func TestPrintSyncStatusNamesFilesAndShares(t *testing.T) {
	var sb strings.Builder
	printSyncStatus(&sb, syncStatusReport{
		Session:   "sess",
		NeedBytes: 4 * 1024 * 1024 * 1024,
		NeedFiles: 3,
		Folders: []syncFolderStatus{{
			Folder:    "okdev-sess",
			Local:     "/home/me/repo",
			Remote:    "/workspace",
			Direction: "bi",
			State:     "syncing",
			NeedBytes: 4 * 1024 * 1024 * 1024,
			NeedFiles: 3,
			Pending: []syncPendingFile{
				{Name: "dump.pt", Size: 3900 * 1024 * 1024, Share: 0.92, Direction: "local->remote"},
			},
			Excludes:      []string{".venv", "outputs/**"},
			ExcludeSource: ".stignore",
		}},
	})
	got := sb.String()
	for _, want := range []string{"Pending: 4.0GiB across 3 file(s)", "dump.pt", "92.0%", "local->remote", "excludes in force (2, from .stignore): .venv, outputs/**"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in:\n%s", want, got)
		}
	}
}

func TestPrintSyncStatusConverged(t *testing.T) {
	var sb strings.Builder
	printSyncStatus(&sb, syncStatusReport{Session: "sess", Converged: true, Folders: []syncFolderStatus{{Folder: "okdev-sess"}}})
	if !strings.Contains(sb.String(), "Sync converged") {
		t.Fatalf("unexpected output:\n%s", sb.String())
	}
}

func TestSummarizeLargeSyncEntriesIsSelfContained(t *testing.T) {
	root := t.TempDir()
	big := make([]byte, largeSyncWarnThreshold+1)
	if err := os.WriteFile(filepath.Join(root, "dump.pt"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	got := summarizeLargeSyncEntries(root, 4*1024*1024*1024)
	// The names must travel with the warning: "see earlier logs" was useless
	// as soon as the output scrolled or was captured (#215).
	if !strings.Contains(got, "dump.pt") {
		t.Fatalf("expected the offending file inline, got %q", got)
	}
	if !strings.Contains(got, "okdev sync status") {
		t.Fatalf("expected a pointer to the re-queryable command, got %q", got)
	}
	if strings.Contains(got, "see earlier logs") {
		t.Fatalf("the un-actionable pointer must be gone, got %q", got)
	}
	// An empty tree still produces a self-contained line.
	empty := summarizeLargeSyncEntries(t.TempDir(), 1024)
	if !strings.Contains(empty, "okdev sync status") {
		t.Fatalf("expected the fallback to stay actionable, got %q", empty)
	}
}
