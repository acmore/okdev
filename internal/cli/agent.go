package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	agentcatalog "github.com/acmore/okdev/internal/agent"
	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/output"
	syncengine "github.com/acmore/okdev/internal/sync"
	"github.com/spf13/cobra"
)

type agentExecClient interface {
	ExecShInContainer(context.Context, string, string, string, string) ([]byte, error)
	CopyToPodInContainer(context.Context, string, string, string, string, string) error
}

type agentListRow struct {
	Name       string `json:"name"`
	Binary     string `json:"binary"`
	Installed  bool   `json:"installed"`
	AuthEnv    string `json:"authEnv,omitempty"`
	AuthPath   string `json:"authPath,omitempty"`
	AuthStaged bool   `json:"authStaged"`
}

func newAgentCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Manage coding agents in the session container",
	}
	cmd.AddCommand(newAgentListCmd(opts))
	return cmd
}

func newAgentListCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list [session]",
		Short: "List configured agents and install status",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			applySessionArg(opts, args)
			cc, err := resolveCommandContext(opts, resolveSessionName)
			if err != nil {
				return err
			}
			if err := ensureExistingSessionOwnership(opts, cc.kube, cc.namespace, cc.sessionName, true); err != nil {
				return err
			}
			target, err := resolveTargetRef(cmd.Context(), opts, cc.cfg, cc.namespace, cc.sessionName, cc.kube)
			if err != nil {
				return err
			}
			rows, err := configuredAgentStatusRows(cmd.Context(), cc.kube, cc.namespace, target.PodName, target.Container, cc.cfg.Spec.Agents)
			if err != nil {
				return err
			}
			if opts.Output == "json" {
				return outputJSON(cmd.OutOrStdout(), rows)
			}
			if len(rows) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No agents configured")
				return nil
			}
			table := make([][]string, 0, len(rows))
			for _, row := range rows {
				installed := "no"
				if row.Installed {
					installed = "yes"
				}
				authStaged := "no"
				if row.AuthStaged {
					authStaged = "yes"
				}
				table = append(table, []string{row.Name, row.Binary, installed, authStaged})
			}
			output.PrintTable(cmd.OutOrStdout(), []string{"NAME", "BINARY", "INSTALLED", "AUTH STAGED"}, table)
			return nil
		},
	}
	return cmd
}

func configuredAgentStatusRows(ctx context.Context, client agentExecClient, namespace, pod, container string, agents []config.AgentSpec) ([]agentListRow, error) {
	rows := make([]agentListRow, 0, len(agents))
	for _, agent := range agents {
		spec, ok := agentcatalog.Lookup(agent.Name)
		if !ok {
			continue
		}
		installed, err := agentBinaryInstalled(ctx, client, namespace, pod, container, spec.Binary)
		if err != nil {
			return nil, fmt.Errorf("check %s install status: %w", spec.Name, err)
		}
		row := agentListRow{
			Name:      spec.Name,
			Binary:    spec.Binary,
			Installed: installed,
		}
		if agent.Auth != nil {
			row.AuthEnv = strings.TrimSpace(agent.Auth.Env)
			row.AuthPath = strings.TrimSpace(agent.Auth.LocalPath)
		}
		if spec.RemoteAuthPath != "" {
			staged, err := agentAuthStaged(ctx, client, namespace, pod, container, spec.RemoteAuthPath)
			if err != nil {
				return nil, fmt.Errorf("check %s auth status: %w", spec.Name, err)
			}
			row.AuthStaged = staged
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func ensureConfiguredAgentsInstalled(ctx context.Context, client agentExecClient, namespace, pod, container string, agents []config.AgentSpec, warnf func(string, ...any)) []string {
	results := make([]string, 0, len(agents))
	for _, agent := range agents {
		spec, ok := agentcatalog.Lookup(agent.Name)
		if !ok {
			continue
		}
		installed, err := agentBinaryInstalled(ctx, client, namespace, pod, container, spec.Binary)
		if err != nil {
			warnf("failed to check %s install status: %v", spec.Name, err)
			results = append(results, spec.Name+": check failed")
			continue
		}
		if installed {
			results = append(results, spec.Name+": present")
			continue
		}
		npmInstalled, err := agentBinaryInstalled(ctx, client, namespace, pod, container, "npm")
		if err != nil {
			warnf("failed to check npm before installing %s: %v", spec.Name, err)
			results = append(results, spec.Name+": npm check failed")
			continue
		}
		if !npmInstalled {
			warnf("skipping %s install: npm is not available in container %s", spec.Name, container)
			results = append(results, spec.Name+": skipped (npm missing)")
			continue
		}
		if _, err := client.ExecShInContainer(ctx, namespace, pod, container, spec.InstallCommand); err != nil {
			warnf("failed to install %s: %v", spec.Name, err)
			results = append(results, spec.Name+": install failed")
			continue
		}
		installed, err = agentBinaryInstalled(ctx, client, namespace, pod, container, spec.Binary)
		if err != nil || !installed {
			if err != nil {
				warnf("failed to verify %s after install: %v", spec.Name, err)
			} else {
				warnf("%s install completed but binary %q is still unavailable", spec.Name, spec.Binary)
			}
			results = append(results, spec.Name+": verification failed")
			continue
		}
		results = append(results, spec.Name+": installed")
	}
	return results
}

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
	stagePath := path.Join(stageDir, "auth")
	remotePath := strings.TrimSpace(spec.RemoteAuthPath)
	if remotePath == "" {
		return "", fmt.Errorf("remote auth path is not defined")
	}
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
		if !ok || strings.TrimSpace(spec.RemoteAuthPath) == "" {
			continue
		}
		if err := cleanupAgentAuth(ctx, client, namespace, pod, container, spec); err != nil {
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

func cleanupAgentAuth(ctx context.Context, client agentExecClient, namespace, pod, container string, spec agentcatalog.Spec) error {
	stageDir := agentRemoteStageDir(spec.Name)
	remotePath := strings.TrimSpace(spec.RemoteAuthPath)
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
		return "\"$HOME\"/" + shellEscapeBare(strings.TrimPrefix(trimmed, "$HOME/"))
	default:
		return syncengine.ShellEscape(trimmed)
	}
}

func shellEscapeBare(v string) string {
	escaped := syncengine.ShellEscape(v)
	if len(escaped) >= 2 && escaped[0] == '\'' && escaped[len(escaped)-1] == '\'' {
		return escaped[1 : len(escaped)-1]
	}
	return escaped
}
