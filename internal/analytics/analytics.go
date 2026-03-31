package analytics

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/acmore/okdev/internal/version"
	posthog "github.com/posthog/posthog-go"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

var PostHogKey = ""

const (
	envPostHogKey      = "OKDEV_POSTHOG_KEY"
	envPostHogEndpoint = "OKDEV_POSTHOG_ENDPOINT"
	envDisabled        = "OKDEV_ANALYTICS_DISABLED"

	defaultPostHogEndpoint = "https://us.i.posthog.com/capture/"

	analyticsDirName = ".okdev/analytics"
	anonymousIDFile  = "anonymous_id"
	noticeFile       = "notice_shown"

	analyticsShutdownTimeout = 1500 * time.Millisecond
	analyticsUploadTimeout   = 1250 * time.Millisecond
)

type Client struct {
	endpoint string
	apiKey   string

	now           func() time.Time
	homeDir       func() (string, error)
	newPostHogCLI func(string, string) (posthog.Client, error)

	mu     sync.Mutex
	cached string
}

type captureEvent struct {
	Event      string                 `json:"event"`
	DistinctID string                 `json:"distinct_id"`
	Timestamp  time.Time              `json:"timestamp"`
	Properties map[string]interface{} `json:"properties"`
}

func NewFromEnv() *Client {
	if analyticsDisabled() {
		return nil
	}
	apiKey := strings.TrimSpace(os.Getenv(envPostHogKey))
	if apiKey == "" {
		apiKey = PostHogKey
	}
	if apiKey == "" {
		return nil
	}
	endpoint := strings.TrimSpace(os.Getenv(envPostHogEndpoint))
	if endpoint == "" {
		endpoint = defaultPostHogEndpoint
	}
	return &Client{
		endpoint: endpoint,
		apiKey:   apiKey,
		now:      time.Now,
		homeDir:  os.UserHomeDir,
		newPostHogCLI: func(apiKey, endpoint string) (posthog.Client, error) {
			return posthog.NewWithConfig(apiKey, posthog.Config{
				Endpoint:           normalizePostHogEndpoint(endpoint),
				BatchSize:          1,
				Interval:           time.Hour,
				BatchUploadTimeout: analyticsUploadTimeout,
				ShutdownTimeout:    analyticsShutdownTimeout,
			})
		},
	}
}

func analyticsDisabled() bool {
	value := strings.TrimSpace(os.Getenv(envDisabled))
	if value == "1" || strings.EqualFold(value, "true") || strings.EqualFold(value, "yes") {
		return true
	}
	return userConfigDisabled()
}

const userConfigFile = ".okdev/config.yaml"

type userConfig struct {
	Analytics *bool `yaml:"analytics"`
}

func userConfigDisabled() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	data, err := os.ReadFile(filepath.Join(home, userConfigFile))
	if err != nil {
		return false
	}
	var cfg userConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return false
	}
	return cfg.Analytics != nil && !*cfg.Analytics
}

// ShowNotice prints a one-time telemetry notice to stderr the first time
// analytics are active. Subsequent calls are no-ops.
func (c *Client) ShowNotice() {
	if c == nil {
		return
	}
	home, err := c.homeDir()
	if err != nil {
		return
	}
	noticePath := filepath.Join(home, analyticsDirName, noticeFile)
	if _, err := os.Stat(noticePath); err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "Notice: okdev collects anonymous usage telemetry to improve the tool.")
	fmt.Fprintln(os.Stderr, "To disable, set OKDEV_ANALYTICS_DISABLED=1 or add 'analytics: false' to ~/.okdev/config.yaml")
	fmt.Fprintln(os.Stderr)
	dir := filepath.Join(home, analyticsDirName)
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(noticePath, []byte("shown\n"), 0o644)
}

func (c *Client) TrackCommand(command, output string, verbose bool, runErr error, duration time.Duration) {
	if c == nil {
		return
	}
	anonID, err := c.anonymousID()
	if err != nil {
		return
	}
	event := captureEvent{
		Event:      "okdev command completed",
		DistinctID: anonID,
		Timestamp:  c.now().UTC(),
		Properties: map[string]interface{}{
			"$lib":          "okdev",
			"command":       strings.TrimSpace(command),
			"success":       runErr == nil,
			"duration_ms":   duration.Milliseconds(),
			"interactive":   stdoutInteractive(),
			"version":       version.Version,
			"commit":        version.Commit,
			"goos":          runtime.GOOS,
			"goarch":        runtime.GOARCH,
			"ci":            ciEnabled(),
			"output":        strings.TrimSpace(output),
			"verbose":       verbose,
			"distinct_id":   anonID,
			"okdev_command": strings.TrimSpace(command),
		},
	}
	if runErr != nil {
		event.Properties["error_type"] = fmt.Sprintf("%T", runErr)
	}
	_ = c.send(event)
}

func (c *Client) send(event captureEvent) error {
	newClient := c.newPostHogCLI
	if newClient == nil {
		return nil
	}
	cli, err := newClient(c.apiKey, c.endpoint)
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), analyticsShutdownTimeout)
		defer cancel()
		_ = cli.CloseWithContext(closeCtx)
	}()
	err = cli.Enqueue(posthog.Capture{
		DistinctId: event.DistinctID,
		Event:      event.Event,
		Timestamp:  event.Timestamp,
		Properties: posthog.Properties(event.Properties),
	})
	if err != nil {
		return err
	}
	return nil
}

func normalizePostHogEndpoint(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return endpoint
	}
	endpoint = strings.TrimRight(endpoint, "/")
	endpoint = strings.TrimSuffix(endpoint, "/capture")
	return strings.TrimRight(endpoint, "/")
}

func (c *Client) anonymousID() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != "" {
		return c.cached, nil
	}
	path, err := c.anonymousIDPath()
	if err != nil {
		return "", err
	}
	if data, err := os.ReadFile(path); err == nil {
		c.cached = strings.TrimSpace(string(data))
		if c.cached != "" {
			return c.cached, nil
		}
	}
	id, err := randomID()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(id+"\n"), 0o644); err != nil {
		return "", err
	}
	c.cached = id
	return id, nil
}

func (c *Client) anonymousIDPath() (string, error) {
	home, err := c.homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, analyticsDirName, anonymousIDFile), nil
}

func randomID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

func ciEnabled() bool {
	return strings.TrimSpace(os.Getenv("CI")) != ""
}

func stdoutInteractive() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}
