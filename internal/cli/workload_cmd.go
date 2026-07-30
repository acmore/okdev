package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/output"
	"github.com/acmore/okdev/internal/session"
	"github.com/acmore/okdev/internal/workload"
	"github.com/spf13/cobra"
	yamlv3 "gopkg.in/yaml.v3"
)

func newWorkloadCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workload",
		Short: "Inspect and switch the workload this session runs",
		Long: `Switch what the current session runs, without creating a new session.

  okdev use <session>         switches WHICH SESSION commands target
  okdev workload use <name>   switches WHAT the current session runs

Switching replaces the running workload: the session name, sync channel, ports
and SSH alias all stay, and the old workload is deleted. To run two shapes at
the same time, use two sessions instead (okdev up --session other).`,
		Example: `  # See what this config declares, and what is pinned and live
  okdev workload list

  # Declare a new workload and scaffold its manifest
  okdev workload add --name train --type pytorchjob

  # Switch this session to it (applies on the next okdev up)
  okdev workload use train
  okdev up`,
	}
	cmd.AddCommand(newWorkloadListCmd(opts))
	cmd.AddCommand(newWorkloadUseCmd(opts))
	cmd.AddCommand(newWorkloadShowCmd(opts))
	cmd.AddCommand(newWorkloadAddCmd(opts))
	return cmd
}

func newWorkloadListCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the workloads this config declares",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cc, err := resolveCommandContext(opts, resolveSessionName)
			if err != nil {
				return err
			}
			pinned, err := session.LoadWorkloadProfile(cc.sessionName)
			if err != nil {
				return err
			}
			live := liveWorkloadProfile(cmd.Context(), cc)

			rows := make([][]string, 0, len(cc.cfg.Spec.Workloads))
			for _, p := range cc.cfg.Spec.Workloads {
				rows = append(rows, []string{
					p.Name,
					normalizeWorkloadType(p.Type),
					manifestCell(p),
					markCell(p.Name == pinned),
					markCell(p.Name == live),
				})
			}
			output.PrintTable(cmd.OutOrStdout(),
				[]string{"NAME", "TYPE", "MANIFEST", "PINNED", "LIVE"}, rows)
			return nil
		},
	}
}

func manifestCell(p config.WorkloadProfile) string {
	if m := strings.TrimSpace(p.ManifestPath); m != "" {
		return m
	}
	return "(spec.podTemplate)"
}

func markCell(on bool) string {
	if on {
		return "*"
	}
	return "-"
}

// liveWorkloadProfile reads the profile label off the session's running pods.
// A cluster error is not fatal here: `workload list` must still show the
// declared profiles and the pin when the cluster is unreachable.
func liveWorkloadProfile(ctx context.Context, cc *commandContext) string {
	if cc == nil || cc.kube == nil {
		return ""
	}
	pods, err := cc.kube.ListPods(ctx, cc.namespace, false,
		"okdev.io/managed=true,okdev.io/session="+cc.sessionName)
	if err != nil {
		return ""
	}
	for _, pod := range pods {
		if pod.Deleting {
			continue
		}
		if p := strings.TrimSpace(pod.Labels["okdev.io/workload-profile"]); p != "" {
			return p
		}
	}
	return ""
}

func newWorkloadUseCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:               "use <name>",
		Short:             "Switch what the current session runs",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: workloadProfileCompletionFunc(opts),
		RunE: func(cmd *cobra.Command, args []string) error {
			cc, err := resolveCommandContext(opts, resolveSessionName)
			if err != nil {
				return err
			}
			want := strings.TrimSpace(args[0])
			if err := cc.cfg.SelectWorkload(want); err != nil {
				return err
			}
			live := liveWorkloadProfile(cmd.Context(), cc)
			if err := session.SaveWorkloadProfile(cc.sessionName, want); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if live != "" && live != want {
				fmt.Fprintf(out, "workload: %s -> %s\n", live, want)
				fmt.Fprintf(out, "session %s is running %s — the next `okdev up` will delete it and recreate as %s\n",
					cc.sessionName, live, want)
			} else {
				fmt.Fprintf(out, "workload: %s\n", want)
			}
			fmt.Fprintln(out, "run `okdev up` to apply")
			return nil
		},
	}
}

func workloadProfileCompletionFunc(opts *Options) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		cfg, _, err := loadConfigAndNamespace(opts)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return cfg.WorkloadProfileNames(), cobra.ShellCompDirectiveNoFileComp
	}
}

func newWorkloadShowCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:               "show [name]",
		Short:             "Show one workload's resolved settings",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: workloadProfileCompletionFunc(opts),
		RunE: func(cmd *cobra.Command, args []string) error {
			cc, err := resolveCommandContext(opts, resolveSessionName)
			if err != nil {
				return err
			}
			if len(args) == 1 {
				if err := cc.cfg.SelectWorkload(args[0]); err != nil {
					return err
				}
			}
			w := cc.cfg.Spec.Workload
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "name:      %s\n", cc.cfg.SelectedWorkload())
			fmt.Fprintf(out, "type:      %s\n", normalizeWorkloadType(w.Type))
			if m := strings.TrimSpace(w.ManifestPath); m != "" {
				fmt.Fprintf(out, "manifest:  %s\n", m)
				fmt.Fprintf(out, "resolved:  %s\n", workload.ResolveManifestPath(cc.cfgPath, m))
			} else {
				fmt.Fprintln(out, "manifest:  (synthesized from spec.podTemplate)")
			}
			for _, in := range cc.cfg.EffectiveWorkloadInject() {
				path := in.Path
				if strings.TrimSpace(path) == "" {
					path = "(object root)"
				}
				fmt.Fprintf(out, "inject:    %s\n", path)
			}
			if c := strings.TrimSpace(w.Attach.Container); c != "" {
				fmt.Fprintf(out, "attach:    %s\n", c)
			}
			return nil
		},
	}
}

func newWorkloadAddCmd(opts *Options) *cobra.Command {
	var name, workloadType, manifestPath string
	var injectPaths []string
	var force bool

	cmd := &cobra.Command{
		Use:   "add",
		Short: "Declare a new workload and scaffold its manifest",
		Args:  cobra.NoArgs,
		Example: `  # A Job workload, manifest scaffolded at .okdev/job.yaml
  okdev workload add --name batch --type job

  # A PyTorchJob with an explicit inject path
  okdev workload add --name train --type pytorchjob \
    --inject-path spec.pytorchReplicaSpecs.Worker.template`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath, err := config.ResolvePath(opts.ConfigPath)
			if err != nil {
				return err
			}
			name = strings.TrimSpace(name)
			workloadType = strings.TrimSpace(workloadType)
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			switch workloadType {
			case workload.TypePod, workload.TypeJob, workload.TypePyTorchJob, workload.TypeGeneric:
			default:
				return fmt.Errorf("--type must be one of pod, job, pytorchjob, generic, got %q", workloadType)
			}
			if strings.TrimSpace(manifestPath) == "" {
				manifestPath = workloadType + ".yaml"
			}

			written, err := scaffoldWorkloadManifest(cfgPath, workloadType, manifestPath, name, force)
			if err != nil {
				return err
			}
			profile := config.WorkloadProfile{Name: name, Type: workloadType, ManifestPath: manifestPath}
			for _, p := range injectPaths {
				profile.Inject = append(profile.Inject, config.WorkloadInjectSpec{Path: strings.TrimSpace(p)})
			}
			if err := appendWorkloadProfileToConfig(cfgPath, profile); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if written != "" {
				fmt.Fprintf(out, "Wrote %s\n", written)
			}
			fmt.Fprintf(out, "Declared workload %q in %s\n", name, cfgPath)
			fmt.Fprintf(out, "next: okdev workload use %s && okdev up\n", name)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Workload name")
	cmd.Flags().StringVar(&workloadType, "type", "", "Workload type: pod, job, pytorchjob, generic")
	cmd.Flags().StringVar(&manifestPath, "manifest-path", "", "Manifest path (defaults to <type>.yaml)")
	cmd.Flags().StringArrayVar(&injectPaths, "inject-path", nil, "Inject path (repeatable)")
	cmd.Flags().BoolVar(&force, "force", false, "Overwrite an existing manifest")
	return cmd
}

// scaffoldWorkloadManifest renders the same embedded manifest templates
// `okdev init` uses. It returns the path written, or "" when the type has no
// template and the user is expected to supply the manifest.
func scaffoldWorkloadManifest(cfgPath, workloadType, manifestPath, name string, force bool) (string, error) {
	var asset string
	switch workloadType {
	case workload.TypeJob:
		asset = "templates/manifests/job.yaml.tmpl"
	case workload.TypePyTorchJob:
		asset = "templates/manifests/pytorchjob.yaml.tmpl"
	case workload.TypePod, workload.TypeGeneric:
		return "", nil
	}
	target := workload.ResolveManifestPath(cfgPath, manifestPath)
	if _, err := os.Stat(target); err == nil && !force {
		return "", fmt.Errorf("manifest already exists at %q (use --force to overwrite)", target)
	}
	vars := config.NewTemplateVars()
	vars.Name = name
	vars.WorkloadType = workloadType
	vars.ManifestPath = manifestPath
	rendered, err := config.RenderEmbeddedTemplate(asset, vars)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", fmt.Errorf("create manifest directory: %w", err)
	}
	if err := os.WriteFile(target, []byte(rendered), 0o644); err != nil {
		return "", fmt.Errorf("write manifest %q: %w", target, err)
	}
	return target, nil
}

// appendWorkloadProfileToConfig adds a profile to spec.workloads in place.
//
// It edits the YAML node tree rather than round-tripping through the config
// struct: a full unmarshal/marshal would silently strip the user's comments and
// reorder their keys.
func appendWorkloadProfileToConfig(cfgPath string, p config.WorkloadProfile) error {
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return fmt.Errorf("read config %q: %w", cfgPath, err)
	}
	var doc yamlv3.Node
	if err := yamlv3.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse config %q: %w", cfgPath, err)
	}
	root := yamlDocumentRoot(&doc)
	if root == nil || root.Kind != yamlv3.MappingNode {
		return fmt.Errorf("config %q is not a mapping", cfgPath)
	}
	spec := ensureYAMLMapping(root, "spec")

	workloads := findYAMLNode(spec, "workloads")
	if workloads == nil {
		workloads = &yamlv3.Node{Kind: yamlv3.SequenceNode}
		// Materialize the workload this config already runs as the first
		// profile, or declaring a second one silently drops the first.
		//
		// It is materialized even when spec.workload is absent: `okdev init`
		// omits that block entirely for pod configs, since pod is the default,
		// and the pod workload is no less real for being implicit.
		existing := &yamlv3.Node{Kind: yamlv3.MappingNode}
		setYAMLNode(existing, "name", yamlScalar(config.DefaultWorkloadProfileName))
		if legacy := findYAMLNode(spec, "workload"); legacy != nil {
			for _, key := range []string{"type", "manifestPath", "inject", "attach"} {
				if v := findYAMLNode(legacy, key); v != nil {
					setYAMLNode(existing, key, v)
				}
			}
		}
		if findYAMLNode(existing, "type") == nil {
			setYAMLNode(existing, "type", yamlScalar(workload.TypePod))
		}
		workloads.Content = append(workloads.Content, existing)
		setYAMLNode(spec, "workloads", workloads)
	}
	if workloads.Kind != yamlv3.SequenceNode {
		return fmt.Errorf("spec.workloads in %q is not a list", cfgPath)
	}
	for _, entry := range workloads.Content {
		if name := findYAMLNode(entry, "name"); name != nil && strings.TrimSpace(name.Value) == strings.TrimSpace(p.Name) {
			return fmt.Errorf("workload %q already exists in %s", p.Name, cfgPath)
		}
	}

	entry := &yamlv3.Node{Kind: yamlv3.MappingNode}
	setYAMLNode(entry, "name", yamlScalar(p.Name))
	setYAMLNode(entry, "type", yamlScalar(p.Type))
	if strings.TrimSpace(p.ManifestPath) != "" {
		setYAMLNode(entry, "manifestPath", yamlScalar(p.ManifestPath))
	}
	if len(p.Inject) > 0 {
		injects := &yamlv3.Node{Kind: yamlv3.SequenceNode}
		for _, in := range p.Inject {
			node := &yamlv3.Node{Kind: yamlv3.MappingNode}
			setYAMLNode(node, "path", yamlScalar(in.Path))
			injects.Content = append(injects.Content, node)
		}
		setYAMLNode(entry, "inject", injects)
	}
	workloads.Content = append(workloads.Content, entry)

	out, err := yamlv3.Marshal(&doc)
	if err != nil {
		return fmt.Errorf("render config %q: %w", cfgPath, err)
	}
	if err := os.WriteFile(cfgPath, out, 0o644); err != nil {
		return fmt.Errorf("write config %q: %w", cfgPath, err)
	}
	return nil
}
