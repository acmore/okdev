package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/acmore/okdev/internal/config"
	"k8s.io/client-go/tools/clientcmd"
)

// InitOverrides holds flag-provided values that skip prompting.
type InitOverrides struct {
	Name          string
	Namespace     string
	KubeContext   string
	WorkloadType  string
	ManifestPath  string
	InjectPaths   []string
	GenericPreset string
	DevImage      string
	SidecarImage  string
	SyncLocal     string
	SyncRemote    string
	SSHUser       string
}

// applyOverrides applies non-empty flag values to template vars.
func applyOverrides(vars *config.TemplateVars, o InitOverrides) {
	if o.Name != "" {
		vars.Name = o.Name
	}
	if o.Namespace != "" {
		vars.Namespace = o.Namespace
	}
	if o.KubeContext != "" {
		vars.KubeContext = o.KubeContext
	}
	if o.WorkloadType != "" {
		vars.WorkloadType = o.WorkloadType
	}
	if o.ManifestPath != "" {
		vars.ManifestPath = o.ManifestPath
	}
	if len(o.InjectPaths) > 0 {
		vars.InjectPaths = append([]string(nil), o.InjectPaths...)
	}
	if o.GenericPreset != "" {
		vars.GenericPreset = o.GenericPreset
	}
	if o.DevImage != "" {
		vars.DevImage = o.DevImage
	}
	if o.SidecarImage != "" {
		vars.SidecarImage = o.SidecarImage
	}
	if o.SyncLocal != "" {
		vars.SyncLocal = o.SyncLocal
	}
	if o.SyncRemote != "" {
		vars.SyncRemote = o.SyncRemote
	}
	if o.SSHUser != "" {
		vars.SSHUser = o.SSHUser
	}
}

// detectDefaultName returns a sensible default for the environment name
// based on the current directory (git repo basename).
func detectDefaultName() string {
	wd, err := os.Getwd()
	if err != nil {
		return "my-project"
	}
	return filepath.Base(wd)
}

func (o InitOverrides) hasName() bool          { return o.Name != "" }
func (o InitOverrides) hasNamespace() bool     { return o.Namespace != "" }
func (o InitOverrides) hasKubeContext() bool   { return o.KubeContext != "" }
func (o InitOverrides) hasWorkloadType() bool  { return o.WorkloadType != "" }
func (o InitOverrides) hasManifestPath() bool  { return o.ManifestPath != "" }
func (o InitOverrides) hasInjectPaths() bool   { return len(o.InjectPaths) > 0 }
func (o InitOverrides) hasGenericPreset() bool { return o.GenericPreset != "" }
func (o InitOverrides) hasSyncLocal() bool     { return o.SyncLocal != "" }
func (o InitOverrides) hasSyncRemote() bool    { return o.SyncRemote != "" }
func (o InitOverrides) hasSSHUser() bool       { return o.SSHUser != "" }

// promptInteractive runs interactive prompts to fill in template vars.
// Only prompts for values not already set by flags.
// When nonInteractive is true, uses defaults without prompting.
func promptInteractive(vars *config.TemplateVars, overrides InitOverrides, in io.Reader, out io.Writer, nonInteractive bool, interactive bool) error {
	if vars.Name == "" {
		vars.Name = detectDefaultName()
	}
	applyKubeDefaults(vars, overrides)
	if nonInteractive {
		return nil
	}
	if !interactive {
		return fmt.Errorf("interactive init requires a TTY; rerun with --yes or pass explicit flags")
	}

	reader := bufio.NewReader(in)

	if !overrides.hasName() {
		input, err := promptString(reader, out, "Environment name (used for session labels and default naming)", vars.Name)
		if err != nil {
			return err
		}
		if input != "" {
			vars.Name = input
		}
	}

	if !overrides.hasNamespace() {
		input, err := promptString(reader, out, "Namespace (where the dev workload will run)", vars.Namespace)
		if err != nil {
			return err
		}
		if input != "" {
			vars.Namespace = input
		}
	}

	if !overrides.hasKubeContext() {
		input, err := promptString(reader, out, "Kube context (kubeconfig context to use)", vars.KubeContext)
		if err != nil {
			return err
		}
		if input != "" {
			vars.KubeContext = input
		}
	}

	if !overrides.hasWorkloadType() {
		input, err := promptWorkloadType(reader, out, vars.WorkloadType)
		if err != nil {
			return err
		}
		if input != "" {
			vars.WorkloadType = input
		}
	}

	if vars.WorkloadType == "generic" && !overrides.hasGenericPreset() {
		input, err := promptString(reader, out, "Generic scaffold preset (optional, e.g. deployment)", vars.GenericPreset)
		if err != nil {
			return err
		}
		if input != "" {
			vars.GenericPreset = input
		}
	}

	applyWorkloadDefaults(vars)

	if vars.WorkloadType == "generic" && !overrides.hasManifestPath() {
		input, err := promptString(reader, out, "Generic manifest path (local YAML to apply)", vars.ManifestPath)
		if err != nil {
			return err
		}
		if input != "" {
			vars.ManifestPath = input
		}
	}

	if vars.WorkloadType == "generic" && !overrides.hasInjectPaths() {
		input, err := promptString(reader, out, "Generic inject paths (comma-separated Kubernetes spec paths)", strings.Join(vars.InjectPaths, ","))
		if err != nil {
			return err
		}
		if input != "" {
			vars.InjectPaths = splitCommaList(input)
		}
	}

	if !overrides.hasSyncLocal() {
		input, err := promptString(reader, out, "Sync local path (project directory on this machine)", vars.SyncLocal)
		if err != nil {
			return err
		}
		if input != "" {
			vars.SyncLocal = input
		}
	}

	if !overrides.hasSyncRemote() {
		input, err := promptString(reader, out, "Sync remote path (workspace path in the container)", vars.SyncRemote)
		if err != nil {
			return err
		}
		if input != "" {
			vars.SyncRemote = input
		}
	}

	if !overrides.hasSSHUser() {
		input, err := promptString(reader, out, "SSH user (login user inside the dev container)", vars.SSHUser)
		if err != nil {
			return err
		}
		if input != "" {
			vars.SSHUser = input
		}
	}

	return nil
}

func promptTemplateVars(reader *bufio.Reader, out io.Writer, meta *config.TemplateMeta, resolved map[string]any) (map[string]any, error) {
	if resolved == nil {
		resolved = map[string]any{}
	}
	for _, v := range meta.Variables {
		if _, ok := resolved[v.Name]; ok {
			continue
		}
		defStr := ""
		if v.HasDefault() {
			defStr = fmt.Sprintf("%v", v.Default)
		}
		label := v.Description
		if label == "" {
			label = v.Name
		}
		input, err := promptString(reader, out, fmt.Sprintf("%s (%s)", label, v.Name), defStr)
		if err != nil {
			return nil, err
		}
		if input == "" && v.HasDefault() {
			resolved[v.Name] = v.Default
			continue
		}
		if input == "" {
			return nil, fmt.Errorf("variable %q is required", v.Name)
		}
		coerced, err := config.CoerceVariableValue(v, input)
		if err != nil {
			return nil, err
		}
		resolved[v.Name] = coerced
	}
	return resolved, nil
}

// promptString prints a prompt with a default and reads user input.
// Returns empty string if user accepts default (just presses Enter).
func promptString(reader *bufio.Reader, out io.Writer, label, defaultVal string) (string, error) {
	if _, err := fmt.Fprintf(out, "? %s: (%s) ", label, defaultVal); err != nil {
		return "", err
	}
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read %s: %w", strings.ToLower(label), err)
	}
	return strings.TrimSpace(line), nil
}

func promptWorkloadType(reader *bufio.Reader, out io.Writer, defaultVal string) (string, error) {
	for {
		input, err := promptString(reader, out, "Workload type (pod=simple dev pod, job=batch workload, pytorchjob=distributed training, generic=custom manifest)", defaultVal)
		if err != nil {
			return "", err
		}
		if input == "" {
			return defaultVal, nil
		}
		switch strings.TrimSpace(input) {
		case "pod", "job", "pytorchjob", "generic":
			return strings.TrimSpace(input), nil
		default:
			if _, err := fmt.Fprintf(out, "invalid workload type %q; choose pod, job, pytorchjob, or generic\n", input); err != nil {
				return "", err
			}
		}
	}
}

// applyKubeDefaults detects the active kubeconfig context and namespace and
// uses them as defaults when no flag override was provided.
func applyKubeDefaults(vars *config.TemplateVars, overrides InitOverrides) {
	kubeCtx, ns := detectKubeDefaults()
	if !overrides.hasKubeContext() && kubeCtx != "" {
		vars.KubeContext = kubeCtx
	}
	if !overrides.hasNamespace() && ns != "" {
		vars.Namespace = ns
	}
}

// detectKubeDefaults reads the active kubeconfig and returns the current
// context name and its configured namespace. Returns empty strings on failure.
func detectKubeDefaults() (kubeContext string, namespace string) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}
	cfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides)

	rawCfg, err := cfg.RawConfig()
	if err != nil {
		return "", ""
	}
	kubeContext = rawCfg.CurrentContext
	if ctx, ok := rawCfg.Contexts[kubeContext]; ok && ctx.Namespace != "" {
		namespace = ctx.Namespace
	}
	return kubeContext, namespace
}

func splitCommaList(input string) []string {
	parts := strings.Split(input, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
