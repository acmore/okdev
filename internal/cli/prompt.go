package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/acmore/okdev/internal/config"
)

// InitOverrides holds flag-provided values that skip prompting.
type InitOverrides struct {
	Name      string
	Namespace string
}

// applyOverrides applies non-empty flag values to template vars.
func applyOverrides(vars *config.TemplateVars, o InitOverrides) {
	if o.Name != "" {
		vars.Name = o.Name
	}
	if o.Namespace != "" {
		vars.Namespace = o.Namespace
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

// promptInteractive runs interactive prompts to fill in template vars.
// Only prompts for values not already set by flags.
// When nonInteractive is true, uses defaults without prompting.
func promptInteractive(vars *config.TemplateVars, nonInteractive bool) error {
	if vars.Name == "" {
		vars.Name = detectDefaultName()
	}
	if nonInteractive {
		return nil
	}

	// Interactive prompts using basic fmt.Scanln.
	// Each prompt shows the current default and allows the user to accept or override.
	var input string

	input = promptString("Environment name", vars.Name)
	if input != "" {
		vars.Name = input
	}

	input = promptString("Namespace", vars.Namespace)
	if input != "" {
		vars.Namespace = input
	}

	input = promptString("Sidecar image", vars.SidecarImage)
	if input != "" {
		vars.SidecarImage = input
	}

	input = promptString("Sync local path", vars.SyncLocal)
	if input != "" {
		vars.SyncLocal = input
	}

	input = promptString("Sync remote path", vars.SyncRemote)
	if input != "" {
		vars.SyncRemote = input
	}

	input = promptString("SSH user", vars.SSHUser)
	if input != "" {
		vars.SSHUser = input
	}

	return nil
}

// promptString prints a prompt with a default and reads user input.
// Returns empty string if user accepts default (just presses Enter).
func promptString(label, defaultVal string) string {
	fmt.Printf("? %s: (%s) ", label, defaultVal)
	var input string
	fmt.Scanln(&input)
	return input
}
