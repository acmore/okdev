package session

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/acmore/okdev/internal/workload"
)

const (
	stateDirName       = ".okdev"
	workspacesDirName  = "workspaces"
	sessionsDirName    = "sessions"
	sessionFileName    = "active_session"
	targetStateName    = "target.json"
	sessionInfoName    = "session.json"
	shutdownMarkerName = "shutdown_requested"
	syncthingDirName   = "syncthing"
)

type Info struct {
	Name         string    `json:"name"`
	RepoRoot     string    `json:"repoRoot,omitempty"`
	ConfigPath   string    `json:"configPath,omitempty"`
	Namespace    string    `json:"namespace,omitempty"`
	KubeContext  string    `json:"kubeContext,omitempty"`
	Owner        string    `json:"owner,omitempty"`
	WorkloadType string    `json:"workloadType,omitempty"`
	CreatedAt    time.Time `json:"createdAt,omitempty"`
	LastUsedAt   time.Time `json:"lastUsedAt,omitempty"`
}

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

func stateRootDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, stateDirName), nil
}

func SessionDir(name string) (string, error) {
	root, err := stateRootDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, sessionsDirName, sanitize(name))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create session directory: %w", err)
	}
	return dir, nil
}

func SyncthingDir(name string) (string, error) {
	dir, err := SessionDir(name)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, syncthingDirName)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return "", fmt.Errorf("create syncthing directory: %w", err)
	}
	return path, nil
}

func SessionInfoPath(name string) (string, error) {
	dir, err := SessionDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, sessionInfoName), nil
}

func ActiveSessionPath() (string, error) {
	root, err := workspaceStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, sessionFileName), nil
}

func workspaceStateDir() (string, error) {
	root, err := stateRootDir()
	if err != nil {
		return "", err
	}
	repoRoot, err := RepoRoot()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, workspacesDirName, repoStateKey(repoRoot))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create workspace state directory: %w", err)
	}
	return dir, nil
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
		return fmt.Errorf("create workspace state directory: %w", err)
	}
	if err := os.WriteFile(p, []byte(name+"\n"), 0o644); err != nil {
		return fmt.Errorf("write active session: %w", err)
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
	return nil
}

func TargetStatePath(name string) (string, error) {
	dir, err := SessionDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, targetStateName), nil
}

func SaveTarget(name string, target workload.TargetRef) error {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	p, err := TargetStatePath(name)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(target)
	if err != nil {
		return fmt.Errorf("marshal target state: %w", err)
	}
	if err := os.WriteFile(p, append(payload, '\n'), 0o644); err != nil {
		return fmt.Errorf("write target state: %w", err)
	}
	return nil
}

func LoadTarget(name string) (workload.TargetRef, error) {
	if strings.TrimSpace(name) == "" {
		return workload.TargetRef{}, nil
	}
	p, err := TargetStatePath(name)
	if err != nil {
		return workload.TargetRef{}, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return workload.TargetRef{}, nil
		}
		return workload.TargetRef{}, fmt.Errorf("read target state: %w", err)
	}
	var target workload.TargetRef
	if err := json.Unmarshal(b, &target); err != nil {
		return workload.TargetRef{}, fmt.Errorf("decode target state: %w", err)
	}
	return target, nil
}

func ClearTarget(name string) error {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	p, err := TargetStatePath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear target state: %w", err)
	}
	return nil
}

func SaveInfo(info Info) error {
	name := sanitize(info.Name)
	if strings.TrimSpace(name) == "" {
		return nil
	}
	path, err := SessionInfoPath(name)
	if err != nil {
		return err
	}
	existing, err := LoadInfo(name)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if existing.CreatedAt.IsZero() {
		existing.CreatedAt = now
	}
	if existing.Name == "" {
		existing.Name = name
	}
	if strings.TrimSpace(info.Name) != "" {
		existing.Name = name
	}
	if strings.TrimSpace(info.RepoRoot) != "" {
		existing.RepoRoot = info.RepoRoot
	}
	if strings.TrimSpace(info.ConfigPath) != "" {
		existing.ConfigPath = info.ConfigPath
	}
	if strings.TrimSpace(info.Namespace) != "" {
		existing.Namespace = info.Namespace
	}
	if strings.TrimSpace(info.KubeContext) != "" {
		existing.KubeContext = info.KubeContext
	}
	if strings.TrimSpace(info.Owner) != "" {
		existing.Owner = info.Owner
	}
	if strings.TrimSpace(info.WorkloadType) != "" {
		existing.WorkloadType = info.WorkloadType
	}
	existing.LastUsedAt = now

	payload, err := json.Marshal(existing)
	if err != nil {
		return fmt.Errorf("marshal session info: %w", err)
	}
	if err := os.WriteFile(path, append(payload, '\n'), 0o644); err != nil {
		return fmt.Errorf("write session info: %w", err)
	}
	return nil
}

func LoadInfo(name string) (Info, error) {
	if strings.TrimSpace(name) == "" {
		return Info{}, nil
	}
	path, err := SessionInfoPath(name)
	if err != nil {
		return Info{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Info{}, nil
		}
		return Info{}, fmt.Errorf("read session info: %w", err)
	}
	var info Info
	if err := json.Unmarshal(b, &info); err != nil {
		return Info{}, fmt.Errorf("decode session info: %w", err)
	}
	return info, nil
}

func ShutdownRequestPath(name string) (string, error) {
	dir, err := SessionDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, shutdownMarkerName), nil
}

func RequestShutdown(name string) error {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	p, err := ShutdownRequestPath(name)
	if err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("write shutdown request: %w", err)
	}
	return nil
}

func ShutdownRequested(name string) (bool, error) {
	if strings.TrimSpace(name) == "" {
		return false, nil
	}
	p, err := ShutdownRequestPath(name)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(p)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("check shutdown request: %w", err)
}

func ClearShutdownRequest(name string) error {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	p, err := ShutdownRequestPath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear shutdown request: %w", err)
	}
	return nil
}
