package cli

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"

	"github.com/acmore/okdev/internal/config"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
	sigs_yaml "sigs.k8s.io/yaml"
)

func newMigrateCmd(opts *Options) *cobra.Command {
	var dryRun bool
	var noBackup bool
	var templateRef string
	var setFlags []string
	var yes bool

	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Migrate a .okdev.yaml config to the latest schema",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath, err := config.ResolvePath(opts.ConfigPath)
			if err != nil {
				return err
			}

			if strings.TrimSpace(templateRef) != "" {
				return runMigrateTemplate(cmd, cfgPath, templateRef, setFlags, yes, dryRun, noBackup)
			}

			raw, err := os.ReadFile(cfgPath)
			if err != nil {
				return fmt.Errorf("read config %q: %w", cfgPath, err)
			}

			var doc yaml.Node
			if err := yaml.Unmarshal(raw, &doc); err != nil {
				return fmt.Errorf("parse config %q: %w", cfgPath, err)
			}

			result, err := config.RunMigrations(&doc, config.DefaultMigrations)
			if err != nil {
				return fmt.Errorf("migration failed: %w", err)
			}

			if len(result.Applied) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "Config is already up to date.")
				return nil
			}

			out, err := yaml.Marshal(&doc)
			if err != nil {
				return fmt.Errorf("marshal migrated config: %w", err)
			}

			w := cmd.OutOrStdout()

			for _, name := range result.Applied {
				fmt.Fprintf(w, "✓ %s\n", name)
			}
			for _, warning := range result.Warnings {
				fmt.Fprintf(w, "  ⚠ %s\n", warning)
			}

			if dryRun {
				fmt.Fprintln(w, "\n--- dry-run output ---")
				_, _ = io.Copy(w, bytes.NewReader(out))
				return nil
			}

			if !noBackup {
				bakPath := cfgPath + ".bak"
				if err := os.WriteFile(bakPath, raw, 0o644); err != nil {
					return fmt.Errorf("write backup %q: %w", bakPath, err)
				}
			}

			if err := os.WriteFile(cfgPath, out, 0o644); err != nil {
				return fmt.Errorf("write migrated config %q: %w", cfgPath, err)
			}

			fmt.Fprintf(w, "\nWrote migrated config to %s", cfgPath)
			if !noBackup {
				fmt.Fprintf(w, " (backup: %s.bak)", cfgPath)
			}
			fmt.Fprintln(w)
			return nil
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print migrated config to stdout without writing")
	cmd.Flags().BoolVar(&noBackup, "no-backup", false, "Skip creating a .bak backup file")
	cmd.Flags().StringVar(&templateRef, "template", "", "Re-apply a template with merge semantics")
	cmd.Flags().StringArrayVar(&setFlags, "set", nil, "Set a template variable (repeatable: --set key=value)")
	cmd.Flags().BoolVar(&yes, "yes", false, "Non-interactive mode, accept stored/default values")
	return cmd
}

type migrateTemplateResult struct {
	merged  string
	summary string
}

func runMigrateTemplate(cmd *cobra.Command, cfgPath, templateRef string, setFlags []string, yes, dryRun, noBackup bool) error {
	warnMigrateUnknownSets(cmd.ErrOrStderr(), templateRef, setFlags, config.RootDir(cfgPath))
	result, err := mergeTemplateConfig(cfgPath, templateRef, setFlags, nil, config.RootDir(cfgPath), yes)
	if err != nil {
		return err
	}

	w := cmd.OutOrStdout()
	fmt.Fprintln(w, result.summary)
	if dryRun {
		fmt.Fprintln(w, "\n--- dry-run output ---")
		fmt.Fprint(w, result.merged)
		return nil
	}
	if !yes {
		ok, err := promptMigrateApply(cmd.InOrStdin(), w)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(w, "Aborted.")
			return nil
		}
	}

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return fmt.Errorf("read config %q: %w", cfgPath, err)
	}
	if !noBackup {
		bakPath := cfgPath + ".bak"
		if err := os.WriteFile(bakPath, raw, 0o644); err != nil {
			return fmt.Errorf("write backup %q: %w", bakPath, err)
		}
	}
	if err := os.WriteFile(cfgPath, []byte(result.merged), 0o644); err != nil {
		return fmt.Errorf("write migrated config %q: %w", cfgPath, err)
	}
	fmt.Fprintf(w, "Wrote migrated config to %s", cfgPath)
	if !noBackup {
		fmt.Fprintf(w, " (backup: %s.bak)", cfgPath)
	}
	fmt.Fprintln(w)
	return nil
}

func promptMigrateApply(in io.Reader, out io.Writer) (bool, error) {
	if _, err := fmt.Fprint(out, "Apply changes? [y/N] "); err != nil {
		return false, err
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

func mergeTemplateConfig(cfgPath, templateRef string, setFlags []string, storedVars map[string]any, projectDir string, nonInteractive bool) (*migrateTemplateResult, error) {
	existingRaw, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("read existing config: %w", err)
	}
	var existingCfg config.DevEnvironment
	if err := sigs_yaml.Unmarshal(existingRaw, &existingCfg); err != nil {
		return nil, fmt.Errorf("parse existing config: %w", err)
	}
	existingCfg.SetDefaults()
	if storedVars == nil && existingCfg.Spec.Template != nil {
		storedVars = existingCfg.Spec.Template.Vars
	}

	rawTemplate, err := config.ResolveTemplateFromDir(context.Background(), templateRef, projectDir)
	if err != nil {
		return nil, err
	}
	meta, _, err := config.ParseFrontmatter(rawTemplate)
	if err != nil {
		return nil, err
	}
	customVars, err := config.ResolveVariables(meta, parseSetFlags(setFlags), storedVars)
	if err != nil {
		return nil, err
	}

	vars := config.NewTemplateVars()
	vars.Name = existingCfg.Metadata.Name
	vars.Namespace = existingCfg.Spec.Namespace
	rendered, err := config.RenderTemplateWithVars(context.Background(), templateRef, vars, customVars, projectDir)
	if err != nil {
		return nil, err
	}

	var templateMap map[string]any
	if err := sigs_yaml.Unmarshal([]byte(rendered), &templateMap); err != nil {
		return nil, fmt.Errorf("parse rendered template config: %w", err)
	}
	var existingMap map[string]any
	if err := sigs_yaml.Unmarshal(existingRaw, &existingMap); err != nil {
		return nil, fmt.Errorf("parse existing config map: %w", err)
	}

	merged, preserved := mergeMaps(templateMap, existingMap, "")
	spec, ok := merged["spec"].(map[string]any)
	if !ok {
		spec = map[string]any{}
		merged["spec"] = spec
	}
	templateBlock := map[string]any{"name": strings.TrimSpace(templateRef)}
	if len(customVars) > 0 {
		templateBlock["vars"] = customVars
	}
	spec["template"] = templateBlock

	out, err := sigs_yaml.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("marshal merged config: %w", err)
	}
	var validationCfg config.DevEnvironment
	if err := sigs_yaml.Unmarshal(out, &validationCfg); err != nil {
		return nil, fmt.Errorf("parse merged config for validation: %w", err)
	}
	validationCfg.SetDefaults()
	if err := validationCfg.Validate(); err != nil {
		return nil, fmt.Errorf("merged config is invalid: %w", err)
	}
	summary := "Template migration summary: added template fields and preserved existing values."
	if preserved > 0 {
		summary = fmt.Sprintf("Template migration summary: preserved %d existing value(s); added missing template fields.\nNote: spec.template.vars records the new variable values, but preserved config fields retain their prior values.", preserved)
	}
	return &migrateTemplateResult{merged: string(out), summary: summary}, nil
}

func warnMigrateUnknownSets(w io.Writer, templateRef string, setFlags []string, projectDir string) {
	if len(setFlags) == 0 {
		return
	}
	raw, err := config.ResolveTemplateFromDir(context.Background(), templateRef, projectDir)
	if err != nil {
		return
	}
	meta, _, err := config.ParseFrontmatter(raw)
	if err != nil {
		return
	}
	warnUnknownTemplateSets(w, meta, parseSetFlags(setFlags))
}

func mergeMaps(base, overlay map[string]any, path string) (map[string]any, int) {
	out := make(map[string]any, len(base)+len(overlay))
	for key, val := range base {
		out[key] = val
	}
	preserved := 0
	for key, overlayVal := range overlay {
		childPath := key
		if path != "" {
			childPath = path + "." + key
		}
		if baseVal, ok := out[key]; ok {
			baseMap, baseIsMap := baseVal.(map[string]any)
			overlayMap, overlayIsMap := overlayVal.(map[string]any)
			if baseIsMap && overlayIsMap {
				merged, count := mergeMaps(baseMap, overlayMap, childPath)
				out[key] = merged
				preserved += count
				continue
			}
			if !reflect.DeepEqual(baseVal, overlayVal) {
				preserved++
			}
		}
		out[key] = overlayVal
	}
	return out, preserved
}
