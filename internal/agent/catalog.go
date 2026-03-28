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
		Name:             "claude-code",
		Binary:           "claude",
		DefaultAuthEnv:   "ANTHROPIC_API_KEY",
		DefaultLocalPath: "~/.claude/.credentials.json",
		RemoteAuthPath:   "$HOME/.claude/.credentials.json",
		InstallCommand:   "npm install -g @anthropic-ai/claude-code",
	},
	"codex": {
		Name:             "codex",
		Binary:           "codex",
		DefaultLocalPath: "~/.codex/auth.json",
		RemoteAuthPath:   "$HOME/.codex/auth.json",
		InstallCommand:   "npm install -g @openai/codex",
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
