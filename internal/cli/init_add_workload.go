package cli

import (
	"fmt"
	"strings"
)

// initInvocation is what the user asked for, reduced to the facts that decide
// whether `okdev init` creates a config or appends a workload to one.
type initInvocation struct {
	ConfigExists bool
	WorkloadName string
	Force        bool
	// ProjectFlagsSet names the project-level flags the user actually passed
	// (Cobra's Changed), e.g. "namespace". They configure a project at
	// creation and have no meaning when appending to an existing one.
	ProjectFlagsSet []string
}

// initAdditiveMode reports whether this run appends a workload to an existing
// config instead of creating one.
func initAdditiveMode(inv initInvocation) bool {
	return inv.ConfigExists && !inv.Force && strings.TrimSpace(inv.WorkloadName) != ""
}

// validateInitInvocation rejects incoherent flag combinations before any I/O,
// so a refusal always leaves the project byte-identical.
func validateInitInvocation(inv initInvocation) error {
	name := strings.TrimSpace(inv.WorkloadName)

	if !inv.ConfigExists {
		if name != "" {
			return fmt.Errorf("--workload-name applies when adding a workload to an existing config; a new config's first workload is named %q", "default")
		}
		return nil
	}

	if inv.Force {
		if name != "" {
			// --force rewrites the whole config; appending keeps it. Together
			// they state opposite intents, and silently picking one discards
			// the other.
			return fmt.Errorf("--force rewrites the whole config and --workload-name appends to it; pass only one")
		}
		return nil
	}

	if name == "" {
		return fmt.Errorf("config already exists; pass --workload-name to add a workload to it, or --force to rewrite it")
	}

	if len(inv.ProjectFlagsSet) > 0 {
		return fmt.Errorf("--%s configures a project at creation and cannot be changed by adding a workload; edit the config file instead",
			strings.Join(inv.ProjectFlagsSet, ", --"))
	}
	return nil
}
