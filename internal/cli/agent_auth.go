package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	agentcatalog "github.com/acmore/okdev/internal/agent"
	"github.com/acmore/okdev/internal/config"
	syncengine "github.com/acmore/okdev/internal/sync"
)

func ensureConfiguredAgentAuth(ctx context.Context, client agentExecClient, namespace, pod, container string, shareable bool, agents []config.AgentSpec, warnf func(string, ...any)) []string {
	results := make([]string, 0, len(agents))
	for _, agent := range agents {
		spec, ok := agentcatalog.Lookup(agent.Name)
		if !ok {
			continue
		}
		if shareable {
			warnf("skipping %s auth staging: shareable sessions are not a strong trust boundary", spec.Name)
			results = append(results, spec.Name+": skipped (shareable session)")
			continue
		}
		result, err := ensureAgentAuthStaged(ctx, client, namespace, pod, container, agent, spec)
		if err != nil {
			warnf("failed to stage %s auth: %v", spec.Name, err)
			results = append(results, spec.Name+": staging failed")
			continue
		}
		results = append(results, spec.Name+": "+result)
	}
	return results
}

func agentBinaryInstalled(ctx context.Context, client agentExecClient, namespace, pod, container, binary string) (bool, error) {
	if strings.TrimSpace(binary) == "" {
		return false, nil
	}
	_, err := client.ExecShInContainer(ctx, namespace, pod, container, "command -v "+binary+" >/dev/null 2>&1")
	if err == nil {
		return true, nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "exit code 127") || strings.Contains(msg, "exit status 127") || strings.Contains(msg, "exit code 1") || strings.Contains(msg, "exit status 1") {
		return false, nil
	}
	return false, err
}

func ensureAgentAuthStaged(ctx context.Context, client agentExecClient, namespace, pod, container string, agent config.AgentSpec, spec agentcatalog.Spec) (string, error) {
	if agent.Auth == nil {
		return "no auth configured", nil
	}
	localPath, found, err := resolveAgentLocalAuth(agent.Auth.LocalPath)
	if err != nil {
		return "", err
	}
	if !found {
		if env := strings.TrimSpace(agent.Auth.Env); env != "" && strings.TrimSpace(os.Getenv(env)) != "" {
			return "env configured (manual login still required)", nil
		}
		return "no local auth found", nil
	}
	stageDir := agentRemoteStageDir(spec.Name)
	remotePath := agentRemoteAuthPath(spec.RemoteAuthPath, localPath)
	if remotePath == "" {
		return "", fmt.Errorf("remote auth path is not defined")
	}
	stagePath := path.Join(stageDir, path.Base(remotePath))
	state, err := agentRemoteAuthState(ctx, client, namespace, pod, container, remotePath)
	if err != nil {
		return "", fmt.Errorf("inspect remote auth path: %w", err)
	}
	if state == "file" {
		return "remote auth already present", nil
	}
	setupScript := fmt.Sprintf(
		"mkdir -p %s %s && install -m 600 /dev/null %s",
		syncengine.ShellEscape(stageDir),
		agentRemotePathExpr(path.Dir(remotePath)),
		syncengine.ShellEscape(stagePath),
	)
	if _, err := client.ExecShInContainer(ctx, namespace, pod, container, setupScript); err != nil {
		return "", fmt.Errorf("prepare remote auth paths: %w", err)
	}
	if err := client.CopyToPodInContainer(ctx, namespace, localPath, pod, container, stagePath); err != nil {
		return "", fmt.Errorf("copy auth file: %w", err)
	}
	finalizeScript := fmt.Sprintf(
		"chmod 600 %s && ln -sfn %s %s",
		syncengine.ShellEscape(stagePath),
		syncengine.ShellEscape(stagePath),
		agentRemotePathExpr(remotePath),
	)
	if _, err := client.ExecShInContainer(ctx, namespace, pod, container, finalizeScript); err != nil {
		return "", fmt.Errorf("link remote auth path: %w", err)
	}
	return "auth staged", nil
}

func cleanupConfiguredAgentAuth(ctx context.Context, client agentExecClient, namespace, pod, container string, agents []config.AgentSpec, warnf func(string, ...any)) []string {
	results := make([]string, 0, len(agents))
	for _, agent := range agents {
		spec, ok := agentcatalog.Lookup(agent.Name)
		remotePath := spec.RemoteAuthPath
		if agent.Auth != nil {
			remotePath = agentRemoteAuthPath(spec.RemoteAuthPath, agent.Auth.LocalPath)
		}
		if !ok || strings.TrimSpace(remotePath) == "" {
			continue
		}
		if err := cleanupAgentAuth(ctx, client, namespace, pod, container, spec.Name, remotePath); err != nil {
			warnf("failed to clean %s auth: %v", spec.Name, err)
			results = append(results, spec.Name+": cleanup failed")
			continue
		}
		results = append(results, spec.Name+": auth cleaned")
	}
	return results
}

func resolveAgentLocalAuth(localPath string) (string, bool, error) {
	if strings.TrimSpace(localPath) == "" {
		return "", false, nil
	}
	resolved, err := config.ResolveLocalAgentPath(localPath)
	if err != nil {
		return "", false, err
	}
	info, err := os.Stat(resolved)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if info.IsDir() {
		return "", false, fmt.Errorf("local auth path %q is a directory", resolved)
	}
	return resolved, true, nil
}

func agentAuthStaged(ctx context.Context, client agentExecClient, namespace, pod, container, remotePath string) (bool, error) {
	if strings.TrimSpace(remotePath) == "" {
		return false, nil
	}
	script := "test -f " + agentRemotePathExpr(remotePath)
	_, err := client.ExecShInContainer(ctx, namespace, pod, container, script)
	if err == nil {
		return true, nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "exit code 1") || strings.Contains(msg, "exit status 1") {
		return false, nil
	}
	return false, err
}

func agentRemoteStageDir(agentName string) string {
	return "/run/okdev/agents/" + agentName
}

func agentRemoteAuthPath(defaultRemotePath, localPath string) string {
	remotePath := strings.TrimSpace(defaultRemotePath)
	if remotePath == "" {
		return ""
	}
	localBase := strings.TrimSpace(filepath.Base(strings.TrimSpace(localPath)))
	if localBase == "" || localBase == "." || localBase == string(filepath.Separator) {
		return remotePath
	}
	return path.Join(path.Dir(remotePath), localBase)
}

func agentRemoteAuthState(ctx context.Context, client agentExecClient, namespace, pod, container, remotePath string) (string, error) {
	if strings.TrimSpace(remotePath) == "" {
		return "missing", nil
	}
	script := fmt.Sprintf("if [ -L %s ]; then printf symlink; elif [ -e %s ]; then printf file; else printf missing; fi", agentRemotePathExpr(remotePath), agentRemotePathExpr(remotePath))
	out, err := client.ExecShInContainer(ctx, namespace, pod, container, script)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func cleanupAgentAuth(ctx context.Context, client agentExecClient, namespace, pod, container, agentName, remotePath string) error {
	stageDir := agentRemoteStageDir(agentName)
	remotePath = strings.TrimSpace(remotePath)
	if remotePath == "" {
		return nil
	}
	script := fmt.Sprintf(
		"if [ -L %s ]; then rm -f %s; fi && rm -rf %s",
		agentRemotePathExpr(remotePath),
		agentRemotePathExpr(remotePath),
		syncengine.ShellEscape(stageDir),
	)
	_, err := client.ExecShInContainer(ctx, namespace, pod, container, script)
	return err
}

func agentRemotePathExpr(remotePath string) string {
	trimmed := strings.TrimSpace(remotePath)
	switch {
	case trimmed == "$HOME":
		return "\"$HOME\""
	case strings.HasPrefix(trimmed, "$HOME/"):
		return "\"$HOME\"/" + syncengine.ShellEscape(strings.TrimPrefix(trimmed, "$HOME/"))
	default:
		return syncengine.ShellEscape(trimmed)
	}
}
