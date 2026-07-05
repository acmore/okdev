package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergeManagedSTIgnoreBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".stignore")

	// Creating the block appends after existing user content.
	if err := os.WriteFile(path, []byte("node_modules\n// user comment\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	retained, err := mergeManagedSTIgnoreBlock(dir, []string{"/results"})
	if err != nil || len(retained) != 0 {
		t.Fatalf("merge: retained=%v err=%v", retained, err)
	}
	content, _ := os.ReadFile(path)
	text := string(content)
	if !strings.Contains(text, "node_modules\n") || !strings.Contains(text, "// user comment") {
		t.Fatalf("user content must be preserved, got %q", text)
	}
	if !strings.Contains(text, managedSTIgnoreBegin+"\n/results\n"+managedSTIgnoreEnd) {
		t.Fatalf("expected managed block with /results, got %q", text)
	}

	// Idempotent: same input, same output.
	if _, err := mergeManagedSTIgnoreBlock(dir, []string{"/results"}); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(path)
	if string(again) != text {
		t.Fatalf("merge must be idempotent:\n%q\nvs\n%q", text, again)
	}

	// Growing the active set keeps sorted patterns inside the block.
	if _, err := mergeManagedSTIgnoreBlock(dir, []string{"/datasets", "/results"}); err != nil {
		t.Fatal(err)
	}
	active, tombs := readManagedSTIgnoreBlock(dir)
	if strings.Join(active, ",") != "/datasets,/results" || len(tombs) != 0 {
		t.Fatalf("unexpected block state: active=%v tombs=%v", active, tombs)
	}

	// Dropping a mapping tombstones its pattern instead of releasing the
	// subtree into the primary folder.
	retained, err = mergeManagedSTIgnoreBlock(dir, []string{"/datasets"})
	if err != nil {
		t.Fatal(err)
	}
	if len(retained) != 1 || retained[0] != "/results" {
		t.Fatalf("expected /results newly retained, got %v", retained)
	}
	active, tombs = readManagedSTIgnoreBlock(dir)
	if strings.Join(active, ",") != "/datasets" || strings.Join(tombs, ",") != "/results" {
		t.Fatalf("unexpected block state: active=%v tombs=%v", active, tombs)
	}
	content, _ = os.ReadFile(path)
	if !strings.Contains(string(content), managedSTIgnoreTombstone+"\n/results\n") {
		t.Fatalf("tombstone comment must precede the retained pattern, got %q", content)
	}

	// A tombstoned pattern re-added to the config becomes active again and
	// is reported as retained exactly once.
	retained, err = mergeManagedSTIgnoreBlock(dir, []string{"/datasets", "/results"})
	if err != nil || len(retained) != 0 {
		t.Fatalf("re-adding must not re-report retention: retained=%v err=%v", retained, err)
	}
	active, tombs = readManagedSTIgnoreBlock(dir)
	if strings.Join(active, ",") != "/datasets,/results" || len(tombs) != 0 {
		t.Fatalf("re-added pattern must leave tombstones, got active=%v tombs=%v", active, tombs)
	}

	// User content after the block is preserved across rewrites.
	if err := os.WriteFile(path, []byte(string(content)+"trailing-user-pattern\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mergeManagedSTIgnoreBlock(dir, []string{"/datasets"}); err != nil {
		t.Fatal(err)
	}
	final, _ := os.ReadFile(path)
	if !strings.Contains(string(final), "trailing-user-pattern") {
		t.Fatalf("content after block must survive, got %q", final)
	}
}

func TestMergeManagedSTIgnoreBlockNoopWithoutFileOrPatterns(t *testing.T) {
	dir := t.TempDir()
	if retained, err := mergeManagedSTIgnoreBlock(dir, nil); err != nil || retained != nil {
		t.Fatalf("no file + no patterns must be a no-op, got %v %v", retained, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".stignore")); !os.IsNotExist(err) {
		t.Fatal("no-op must not create .stignore")
	}
}
