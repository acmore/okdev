package session

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

const stateDirName = ".okdev"
const sessionsDirName = "sessions"
const sessionFileName = "active_session"

var (
	repoRootOnce sync.Once
	repoRootVal  string
	repoRootErr  error
)

func RepoRoot() (string, error) {
	repoRootOnce.Do(func() {
		cmd := exec.Command("git", "rev-parse", "--show-toplevel")
		out, err := cmd.Output()
		if err == nil {
			repoRootVal = strings.TrimSpace(string(out))
			return
		}
		wd, werr := os.Getwd()
		if werr != nil {
			repoRootErr = fmt.Errorf("get working directory: %w", werr)
			return
		}
		repoRootVal = wd
	})
	return repoRootVal, repoRootErr
}

func ActiveSessionPath() (string, error) {
	root, err := activeSessionRootDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, sessionFileName), nil
}

func activeSessionRootDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	root, err := RepoRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, stateDirName, sessionsDirName, repoStateKey(root)), nil
}

func legacyActiveSessionPath() (string, error) {
	root, err := RepoRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, stateDirName, sessionFileName), nil
}

func repoStateKey(repoRoot string) string {
	base := filepath.Base(strings.TrimSpace(repoRoot))
	base = strings.TrimSpace(base)
	if base == "" {
		base = "repo"
	}
	sum := sha1.Sum([]byte(repoRoot))
	short := hex.EncodeToString(sum[:])[:12]
	return sanitize(base) + "-" + short
}

func LoadActiveSession() (string, error) {
	p, err := ActiveSessionPath()
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			lp, lerr := legacyActiveSessionPath()
			if lerr != nil {
				return "", lerr
			}
			lb, lreadErr := os.ReadFile(lp)
			if lreadErr != nil {
				if errors.Is(lreadErr, os.ErrNotExist) {
					return "", nil
				}
				return "", fmt.Errorf("read active session (legacy): %w", lreadErr)
			}
			return strings.TrimSpace(string(lb)), nil
		}
		return "", fmt.Errorf("read active session: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

func SaveActiveSession(name string) error {
	p, err := ActiveSessionPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	if err := os.WriteFile(p, []byte(name+"\n"), 0o644); err != nil {
		return fmt.Errorf("write active session: %w", err)
	}
	if lp, lerr := legacyActiveSessionPath(); lerr == nil {
		_ = os.Remove(lp)
		_ = os.Remove(filepath.Dir(lp))
	}
	return nil
}

func ClearActiveSession() error {
	p, err := ActiveSessionPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear active session: %w", err)
	}
	if lp, lerr := legacyActiveSessionPath(); lerr == nil {
		_ = os.Remove(lp)
		_ = os.Remove(filepath.Dir(lp))
	}
	return nil
}
