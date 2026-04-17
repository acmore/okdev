package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var invalidChars = regexp.MustCompile(`[^a-z0-9-]`)

func Resolve(explicit string, template string) (string, error) {
	if explicit != "" {
		return sanitize(explicit), nil
	}
	active, err := LoadActiveSession()
	if err != nil {
		return "", err
	}
	if active != "" {
		return sanitize(active), nil
	}
	return ResolveDefault(template)
}

func ResolveDefault(template string) (string, error) {
	repo, _, user, err := contextValues()
	if err != nil {
		return "", err
	}
	if template == "" {
		template = "{{ .Repo }}-{{ .User }}"
	}
	r := strings.NewReplacer(
		"{{ .Repo }}", repo,
		"{{ .User }}", user,
	)
	name := r.Replace(template)
	return sanitize(name), nil
}

func contextValues() (repo string, branch string, user string, err error) {
	root, err := RepoRoot()
	if err != nil {
		return "", "", "", err
	}
	repo = filepath.Base(root)
	branch = gitBranch()
	if branch == "" {
		branch = "main"
	}
	user = os.Getenv("USER")
	if user == "" {
		user = "dev"
	}
	return sanitize(repo), sanitize(branch), sanitize(user), nil
}

func gitBranch() string {
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func sanitize(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = strings.ReplaceAll(s, "_", "-")
	s = strings.ReplaceAll(s, "/", "-")
	s = invalidChars.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "okdev-session"
	}
	if len(s) > 50 {
		s = s[:50]
		s = strings.Trim(s, "-")
	}
	return s
}

func ValidateNonEmpty(name string) error {
	if sanitize(name) == "" {
		return fmt.Errorf("session name cannot be empty")
	}
	return nil
}
