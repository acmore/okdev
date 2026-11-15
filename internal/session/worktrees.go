package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	worktreeStateDir   = "worktrees"
	WorktreeRemoteBase = "/var/okdev/worktrees"
)

type WorktreeBinding struct {
	ID         string `json:"id"`
	LocalRoot  string `json:"localRoot"`
	RemoteRoot string `json:"remoteRoot"`
}

func WorktreeID(root string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(root)))
	return hex.EncodeToString(sum[:8])
}

func WorktreeRemoteRoot(id string) string {
	return WorktreeRemoteBase + "/" + id
}

// IsWorktreeRemoteRoot reports whether a remote sync root belongs to an
// attached worktree rather than to a mapping from the config.
func IsWorktreeRemoteRoot(path string) bool {
	return strings.HasPrefix(path, WorktreeRemoteBase+"/")
}

func worktreeStatePath(name, id string) (string, error) {
	if len(id) != 16 {
		return "", fmt.Errorf("invalid worktree id %q", id)
	}
	for _, c := range id {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return "", fmt.Errorf("invalid worktree id %q", id)
		}
	}
	dir, err := SessionDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, worktreeStateDir, id+".json"), nil
}

func SaveWorktreeBinding(name string, binding WorktreeBinding) error {
	if binding.ID != WorktreeID(binding.LocalRoot) || binding.RemoteRoot != WorktreeRemoteRoot(binding.ID) {
		return fmt.Errorf("invalid worktree binding")
	}
	path, err := worktreeStatePath(name, binding.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".worktree-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func LoadWorktreeBindings(name string) ([]WorktreeBinding, error) {
	dir, err := SessionDir(name)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(dir, worktreeStateDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	bindings := make([]WorktreeBinding, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, worktreeStateDir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var binding WorktreeBinding
		if err := json.Unmarshal(raw, &binding); err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
		if binding.ID != strings.TrimSuffix(entry.Name(), ".json") || binding.ID != WorktreeID(binding.LocalRoot) || binding.RemoteRoot != WorktreeRemoteRoot(binding.ID) {
			return nil, fmt.Errorf("invalid worktree binding in %s", path)
		}
		bindings = append(bindings, binding)
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].ID < bindings[j].ID })
	return bindings, nil
}

func RemoveWorktreeBinding(name, id string) error {
	path, err := worktreeStatePath(name, id)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// ClearWorktreeBindings drops every attachment of a session. It removes the
// state directory itself, so a binding file that no longer decodes cannot
// survive a teardown.
func ClearWorktreeBindings(name string) error {
	dir, err := SessionDir(name)
	if err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(dir, worktreeStateDir))
}

// worktreeLockPollInterval is how often a blocked AcquireWorktreeLock retries.
const worktreeLockPollInterval = 200 * time.Millisecond

// AcquireWorktreeLock takes the session's worktree lock and returns its
// release function. The lock serializes everything that decides which
// worktree folders the session syncs: attach, detach, and the bootstrap of a
// sync start.
//
// It never blocks invisibly: onWait (optional) is called once if another
// process holds the lock, and the wait ends with ctx. A holder that dies
// releases the lock with its file descriptor.
func AcquireWorktreeLock(ctx context.Context, name string, onWait func()) (func(), error) {
	dir, err := SessionDir(name)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "worktrees.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	waited := false
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return nil, fmt.Errorf("lock worktree state: %w", err)
		}
		if !waited && onWait != nil {
			onWait()
		}
		waited = true
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("waiting for another okdev operation on session %s: %w", name, ctx.Err())
		case <-time.After(worktreeLockPollInterval):
		}
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
		})
	}, nil
}

// WithWorktreeLock runs fn while holding the session's worktree lock.
func WithWorktreeLock(ctx context.Context, name string, onWait func(), fn func() error) error {
	release, err := AcquireWorktreeLock(ctx, name, onWait)
	if err != nil {
		return err
	}
	defer release()
	return fn()
}
