package cli

import (
	"bufio"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	agentcatalog "github.com/acmore/okdev/internal/agent"
	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/session"
	syncengine "github.com/acmore/okdev/internal/sync"
	"github.com/spf13/cobra"
)

// A config is usually committed to the repository it describes, so whoever
// can change the repository can change it. Most of what it controls lives in
// the cluster. A few fields reach the developer's own machine instead: a sync
// root outside the project (bidirectional, so the pod can read and write it),
// local credential files uploaded into the pod, reverse port forwards (the pod
// reaches this machine's localhost), and SSH agent forwarding (the pod can use
// the developer's keys). Each command that exercises one of these needs it
// approved for this config first.

type capabilityKind int

const (
	capSyncOutsideProject capabilityKind = iota
	capCredentialUpload
	capReverseForward
	capAgentForward
)

var allCapabilityKinds = []capabilityKind{capSyncOutsideProject, capCredentialUpload, capReverseForward, capAgentForward}

type localCapability struct {
	kind capabilityKind
	// text is shown to the user and is what an approval records.
	text string
}

// localCapabilities lists what cfg can do on this machine beyond the project
// directory, sorted by text.
func localCapabilities(cfg *config.DevEnvironment, cfgPath string) ([]localCapability, error) {
	if cfg == nil {
		return nil, nil
	}
	root, err := filepath.Abs(config.RootDir(cfgPath))
	if err != nil {
		return nil, err
	}
	var caps []localCapability
	pairs, err := syncengine.ParsePairs(cfg.Spec.Sync.Paths, cfg.EffectiveWorkspaceMountPath(cfgPath))
	if err != nil {
		return nil, err
	}
	for _, pair := range pairs {
		// Resolved exactly as the sync engine resolves it (relative to the
		// working directory), so the approval covers what is actually synced.
		local, err := filepath.Abs(pair.Local)
		if err != nil {
			return nil, err
		}
		if pathContainedIn(root, local) {
			continue
		}
		caps = append(caps, localCapability{capSyncOutsideProject,
			fmt.Sprintf("sync %s (outside the project %s) with pod:%s, direction %s", local, root, pair.Remote, pair.Direction)})
	}
	for _, agent := range cfg.Spec.Agents {
		if agent.Auth == nil || strings.TrimSpace(agent.Auth.LocalPath) == "" {
			continue
		}
		spec, ok := agentcatalog.Lookup(agent.Name)
		if !ok {
			continue
		}
		local, err := config.ResolveLocalAgentPath(agent.Auth.LocalPath)
		if err != nil {
			return nil, err
		}
		caps = append(caps, localCapability{capCredentialUpload,
			fmt.Sprintf("upload %s credentials %s into the pod", spec.Name, local)})
	}
	for _, p := range cfg.Spec.Ports {
		if p.EffectiveDirection() == config.PortDirectionReverse {
			caps = append(caps, localCapability{capReverseForward,
				fmt.Sprintf("let the pod reach this machine's localhost:%d (reverse forward from pod port %d)", p.Local, p.Remote)})
		}
	}
	if cfg.Spec.SSH.ForwardAgentEnabled() {
		caps = append(caps, localCapability{capAgentForward, "forward this machine's SSH agent into the pod"})
	}
	sort.Slice(caps, func(i, j int) bool { return caps[i].text < caps[j].text })
	return caps, nil
}

func pathContainedIn(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// unapprovedCapabilities returns the capabilities of the given kinds that the
// user has not approved for this config, along with the config's absolute
// path and the full capability list (shown when asking).
func unapprovedCapabilities(cfg *config.DevEnvironment, cfgPath string, kinds []capabilityKind) (missing, all []localCapability, absPath string, err error) {
	all, err = localCapabilities(cfg, cfgPath)
	if err != nil || len(all) == 0 {
		return nil, all, "", err
	}
	absPath, err = filepath.Abs(cfgPath)
	if err != nil {
		return nil, nil, "", err
	}
	approvals, err := session.LoadConfigTrust()
	if err != nil {
		return nil, nil, "", err
	}
	approved := map[string]bool{}
	for _, text := range approvals[absPath].Capabilities {
		approved[text] = true
	}
	wanted := map[capabilityKind]bool{}
	for _, kind := range kinds {
		wanted[kind] = true
	}
	for _, c := range all {
		if wanted[c.kind] && !approved[c.text] {
			missing = append(missing, c)
		}
	}
	return missing, all, absPath, nil
}

// approveConfigTrust records the full current capability list, merged with
// what was approved before (an approval never silently shrinks).
func approveConfigTrust(absPath string, caps []localCapability) error {
	approvals, err := session.LoadConfigTrust()
	if err != nil {
		return err
	}
	set := map[string]bool{}
	for _, text := range approvals[absPath].Capabilities {
		set[text] = true
	}
	for _, c := range caps {
		set[c.text] = true
	}
	texts := make([]string, 0, len(set))
	for text := range set {
		texts = append(texts, text)
	}
	sort.Strings(texts)
	return session.SaveConfigTrust(absPath, session.ConfigTrust{Capabilities: texts, ApprovedAt: time.Now().UTC()})
}

func writeCapabilities(w io.Writer, caps []localCapability) {
	for _, c := range caps {
		fmt.Fprintf(w, "  - %s\n", c.text)
	}
}

// requireConfigTrust is the gate in front of every command that exercises
// local capabilities; kinds are the ones that command actually uses. On a
// terminal it asks once (showing everything the config can do); elsewhere it
// refuses and names `okdev trust`, so automation approves explicitly rather
// than through a generic --yes it passes everywhere.
func requireConfigTrust(cmd *cobra.Command, cfg *config.DevEnvironment, cfgPath string, kinds ...capabilityKind) error {
	missing, all, absPath, err := unapprovedCapabilities(cfg, cfgPath, kinds)
	if err != nil {
		return fmt.Errorf("check config trust: %w", err)
	}
	if len(missing) == 0 {
		return nil
	}
	if !isTerminalReader(cmd.InOrStdin()) {
		var b strings.Builder
		writeCapabilities(&b, missing)
		return fmt.Errorf("config %s asks to act on this machine beyond the project:\n%sreview it, then approve with `okdev trust --config %s`", absPath, b.String(), absPath)
	}
	out := cmd.ErrOrStderr()
	fmt.Fprintf(out, "Config %s asks to act on this machine beyond the project:\n", absPath)
	writeCapabilities(out, all)
	fmt.Fprint(out, "Only allow this for a config you wrote or reviewed. Allow? [y/N]: ")
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && line == "" {
		return fmt.Errorf("read confirmation: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return approveConfigTrust(absPath, all)
	}
	return fmt.Errorf("config not approved; nothing was started")
}

func newTrustCmd(opts *Options) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "trust",
		Short: "Review and approve what a config may do on this machine",
		Long: `List what the config can do on this machine beyond its project directory
(sync roots outside the project, local credential uploads, reverse port
forwards, SSH agent forwarding) and approve it.

Commands that use these capabilities (up, restart, sync, ssh, ports) ask on a
terminal and refuse otherwise until they are approved. Approval is recorded
per config path; a capability added later asks again.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, cfgPath, err := config.Load(opts.ConfigPath)
			if err != nil {
				return err
			}
			missing, all, absPath, err := unapprovedCapabilities(cfg, cfgPath, allCapabilityKinds)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(all) == 0 {
				fmt.Fprintf(out, "Config %s does not act on this machine beyond its project; no approval needed.\n", cfgPath)
				return nil
			}
			fmt.Fprintf(out, "Config %s can:\n", absPath)
			writeCapabilities(out, all)
			if len(missing) == 0 {
				fmt.Fprintln(out, "Already approved.")
				return nil
			}
			if !yes {
				if !isTerminalReader(cmd.InOrStdin()) {
					return fmt.Errorf("not approved; re-run with --yes to approve non-interactively")
				}
				fmt.Fprint(out, "Allow? [y/N]: ")
				line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
					return fmt.Errorf("not approved")
				}
			}
			if err := approveConfigTrust(absPath, all); err != nil {
				return err
			}
			fmt.Fprintln(out, "Approved.")
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Approve without prompting")
	return cmd
}
