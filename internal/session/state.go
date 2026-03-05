package session

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const stateDirName = ".okdev"
const sessionFileName = "active_session"

func RepoRoot() (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err == nil {
		return strings.TrimSpace(string(out)), nil
	}
	wd, werr := os.Getwd()
	if werr != nil {
		return "", fmt.Errorf("get working directory: %w", werr)
	}
	return wd, nil
}

func ActiveSessionPath() (string, error) {
	root, err := RepoRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, stateDirName, sessionFileName), nil
}

func LoadActiveSession() (string, error) {
	p, err := ActiveSessionPath()
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
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
	return nil
}
