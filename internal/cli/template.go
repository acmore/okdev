package cli

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/acmore/okdev/internal/config"
	"github.com/spf13/cobra"
)

type templateEntry struct {
	Name        string
	Source      string
	Description string
}

func newTemplateCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "template",
		Short: "Manage workload templates",
	}
	cmd.AddCommand(newTemplateListCmd(opts))
	cmd.AddCommand(newTemplateShowCmd(opts))
	return cmd
}

func newTemplateListCmd(opts *Options) *cobra.Command {
	var projectDir string
	var showAll bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List available templates",
		RunE: func(cmd *cobra.Command, args []string) error {
			if projectDir == "" {
				projectDir = defaultTemplateProjectDir()
			}
			entries := collectTemplateEntries(projectDir, showAll)

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tSOURCE\tDESCRIPTION")
			for _, entry := range entries {
				fmt.Fprintf(w, "%s\t%s\t%s\n", entry.Name, entry.Source, entry.Description)
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&projectDir, "project-dir", "", "Project directory (defaults to cwd)")
	cmd.Flags().BoolVar(&showAll, "all", false, "Show all layers including shadowed templates")
	return cmd
}

func newTemplateShowCmd(opts *Options) *cobra.Command {
	var projectDir string

	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show template details and variables",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if projectDir == "" {
				projectDir = defaultTemplateProjectDir()
			}
			name := args[0]
			raw, err := config.ResolveTemplateFromDir(context.Background(), name, projectDir)
			if err != nil {
				return fmt.Errorf("template %q not found: %w", name, err)
			}
			meta, _, err := config.ParseFrontmatter(raw)
			if err != nil {
				return err
			}
			displayName := meta.Name
			if displayName == "" {
				displayName = name
			}
			desc := meta.Description
			if desc == "" {
				desc = "(no description)"
			}

			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "Name:        %s\n", displayName)
			fmt.Fprintf(w, "Source:      %s\n", resolveTemplateSource(name, projectDir))
			fmt.Fprintf(w, "Description: %s\n", desc)
			if len(meta.Files) > 0 {
				fmt.Fprintln(w, "\nFiles:")
				tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
				for _, file := range meta.Files {
					fmt.Fprintf(tw, "  %s\t%s\n", file.Path, file.Template)
				}
				if err := tw.Flush(); err != nil {
					return err
				}
			}
			if len(meta.Variables) == 0 {
				return nil
			}
			fmt.Fprintln(w, "\nVariables:")
			tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
			for _, v := range meta.Variables {
				typ := v.Type
				if typ == "" {
					typ = "string"
				}
				def := "(required)"
				if v.HasDefault() {
					def = fmt.Sprintf("(default: %v)", v.Default)
				}
				fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", v.Name, typ, v.Description, def)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&projectDir, "project-dir", "", "Project directory (defaults to cwd)")
	return cmd
}

func defaultTemplateProjectDir() string {
	if path, err := config.ResolvePath(""); err == nil {
		return config.RootDir(path)
	}
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd
}

func collectTemplateEntries(projectDir string, showAll bool) []templateEntry {
	seen := map[string]bool{}
	var entries []templateEntry

	if projectDir != "" {
		if names, err := config.ProjectTemplateNames(projectDir); err == nil {
			for _, name := range names {
				seen[name] = true
				entries = append(entries, templateEntry{Name: name, Source: "project", Description: templateDescription(name, projectDir)})
			}
		}
	}
	if names, err := config.UserTemplateNames(); err == nil {
		for _, name := range names {
			if seen[name] && !showAll {
				continue
			}
			source := "user"
			if seen[name] {
				source = "user (shadowed)"
			}
			seen[name] = true
			entries = append(entries, templateEntry{Name: name, Source: source, Description: templateDescription(name, "")})
		}
	}
	for _, name := range config.BuiltinTemplateNames() {
		if seen[name] && !showAll {
			continue
		}
		source := "built-in"
		if seen[name] {
			source = "built-in (shadowed)"
		}
		seen[name] = true
		entries = append(entries, templateEntry{Name: name, Source: source, Description: builtinTemplateDescription(name)})
	}
	return entries
}

func templateDescription(name, projectDir string) string {
	raw, err := config.ResolveTemplateFromDir(context.Background(), name, projectDir)
	if err != nil {
		return "(no description)"
	}
	meta, _, err := config.ParseFrontmatter(raw)
	if err != nil || meta.Description == "" {
		return "(no description)"
	}
	return meta.Description
}

func builtinTemplateDescription(name string) string {
	if name == "basic" {
		return "Basic dev environment"
	}
	return "(no description)"
}

func resolveTemplateSource(name, projectDir string) string {
	if projectDir != "" {
		if names, _ := config.ProjectTemplateNames(projectDir); containsString(names, name) {
			return fmt.Sprintf(".okdev/templates/%s.yaml.tmpl", name)
		}
	}
	if names, _ := config.UserTemplateNames(); containsString(names, name) {
		return fmt.Sprintf("~/.okdev/templates/%s.yaml.tmpl", name)
	}
	return "(built-in)"
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}
