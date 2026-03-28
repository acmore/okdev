package agent

import (
	"slices"
	"strings"
)

type Spec struct {
	Name             string
	Binary           string
	DefaultAuthEnv   string
	DefaultLocalPath string
	RemoteAuthPath   string
	InstallCommand   string
}

var supported = map[string]Spec{
	"claude-code": {
		Name:           "claude-code",
		Binary:         "claude",
		InstallCommand: "npm install -g @anthropic-ai/claude-code",
	},
	"codex": {
		Name:             "codex",
		Binary:           "codex",
		DefaultLocalPath: "~/.codex/auth.json",
		RemoteAuthPath:   "$HOME/.codex/auth.json",
		InstallCommand:   "npm install -g @openai/codex",
	},
	"gemini": {
		Name:           "gemini",
		Binary:         "gemini",
		InstallCommand: "npm install -g @google/gemini-cli",
	},
	"opencode": {
		Name:           "opencode",
		Binary:         "opencode",
		InstallCommand: "npm install -g opencode-ai",
	},
}

func Lookup(name string) (Spec, bool) {
	spec, ok := supported[normalize(name)]
	return spec, ok
}

func SupportedNames() []string {
	names := make([]string, 0, len(supported))
	for name := range supported {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func normalize(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}
