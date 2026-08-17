package cli

import (
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Flags that were removed and now hard-error. Help text that still shows them
// is worse than stale prose: `okdev init --help` recommended
// `okdev init --workload job`, and running exactly that failed with
// "--workload is removed". The error message naming the replacement is the
// correct place to mention them; Short/Long/Example are not.
var removedFlagPatterns = []*regexp.Regexp{
	// --workload, but not --workload-name, which is current.
	regexp.MustCompile(`--workload($|[^-\w])`),
	regexp.MustCompile(`--generic-preset`),
}

func TestHelpTextDoesNotAdvertiseRemovedFlags(t *testing.T) {
	root, _ := newRootCmdWithOptions()
	walkCommands(root, func(cmd *cobra.Command) {
		fields := map[string]string{
			"Short":   cmd.Short,
			"Long":    cmd.Long,
			"Example": cmd.Example,
		}
		for name, text := range fields {
			for _, pattern := range removedFlagPatterns {
				if match := pattern.FindString(text); match != "" {
					t.Errorf("%q %s advertises removed flag %q:\n%s",
						cmd.CommandPath(), name, strings.TrimSpace(match), text)
				}
			}
		}
		cmd.Flags().VisitAll(func(f *pflag.Flag) {
			for _, pattern := range removedFlagPatterns {
				if match := pattern.FindString(f.Usage); match != "" {
					t.Errorf("%q flag --%s usage advertises removed flag %q: %s",
						cmd.CommandPath(), f.Name, strings.TrimSpace(match), f.Usage)
				}
			}
		})
	})
}

// Every example in help text should name a real command and real flags. A
// flag typo'd in an Example is invisible until a user copies it.
func TestHelpExamplesUseDeclaredFlags(t *testing.T) {
	root, _ := newRootCmdWithOptions()
	flagRef := regexp.MustCompile(`--[a-z][a-z0-9-]*`)
	walkCommands(root, func(cmd *cobra.Command) {
		if cmd.Example == "" {
			return
		}
		for _, line := range strings.Split(cmd.Example, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "okdev ") {
				continue
			}
			target := resolveExampleCommand(root, line)
			if target == nil {
				continue
			}
			// Everything after "--" belongs to the remote command, not okdev:
			// `okdev exec --all --script x.sh -- --since 10m` passes --since
			// to the script.
			if idx := strings.Index(line, " -- "); idx >= 0 {
				line = line[:idx]
			}
			for _, ref := range flagRef.FindAllString(line, -1) {
				name := strings.TrimPrefix(ref, "--")
				if target.Flags().Lookup(name) == nil && target.InheritedFlags().Lookup(name) == nil {
					t.Errorf("%q example references undeclared flag --%s (resolved to %q):\n  %s",
						cmd.CommandPath(), name, target.CommandPath(), line)
				}
			}
		}
	})
}

// resolveExampleCommand maps an example line like "okdev workload use train"
// to the command it would actually run, so its flags can be checked.
func resolveExampleCommand(root *cobra.Command, line string) *cobra.Command {
	fields := strings.Fields(line)
	if len(fields) == 0 || fields[0] != "okdev" {
		return nil
	}
	current := root
	for _, field := range fields[1:] {
		if strings.HasPrefix(field, "-") {
			break
		}
		next := findSubcommand(current, field)
		if next == nil {
			break
		}
		current = next
	}
	return current
}

func findSubcommand(parent *cobra.Command, name string) *cobra.Command {
	for _, sub := range parent.Commands() {
		if sub.Name() == name {
			return sub
		}
		for _, alias := range sub.Aliases {
			if alias == name {
				return sub
			}
		}
	}
	return nil
}

func walkCommands(cmd *cobra.Command, visit func(*cobra.Command)) {
	visit(cmd)
	for _, sub := range cmd.Commands() {
		walkCommands(sub, visit)
	}
}
