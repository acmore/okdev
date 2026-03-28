package cli

import (
	"context"
	"fmt"
	"strings"

	agentcatalog "github.com/acmore/okdev/internal/agent"
	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/output"
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
				fmt.Fprintln(cmd.OutOrStdout(), "No agents configured. Add spec.agents to .okdev.yaml to enable agent support.")
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

func summarizeAgentResults(results ...[]string) string {
	grouped := map[string][]string{}
	order := make([]string, 0)
	seen := map[string]struct{}{}
	for _, resultSet := range results {
		for _, result := range resultSet {
			name, detail, ok := strings.Cut(result, ": ")
			if !ok {
				if strings.TrimSpace(result) == "" {
					continue
				}
				name = "_"
				detail = strings.TrimSpace(result)
			}
			if _, exists := seen[name]; !exists {
				order = append(order, name)
				seen[name] = struct{}{}
			}
			grouped[name] = append(grouped[name], detail)
		}
	}
	parts := make([]string, 0, len(order))
	for _, name := range order {
		details := strings.Join(grouped[name], ", ")
		if name == "_" {
			parts = append(parts, details)
			continue
		}
		parts = append(parts, name+": "+details)
	}
	return strings.Join(parts, "; ")
}
