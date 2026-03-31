package analytics

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/acmore/okdev/internal/version"
	"golang.org/x/term"
)

var PostHogKey = ""

const (
	envPostHogKey      = "OKDEV_POSTHOG_KEY"
	envPostHogEndpoint = "OKDEV_POSTHOG_ENDPOINT"
	envDisabled        = "OKDEV_ANALYTICS_DISABLED"

	defaultPostHogEndpoint = "https://us.i.posthog.com/capture/"

	analyticsDirName = ".okdev/analytics"
	anonymousIDFile  = "anonymous_id"
	eventsFile       = "events.jsonl"
)

type Client struct {
	endpoint string
	apiKey   string

	httpClient *http.Client
	now        func() time.Time
	homeDir    func() (string, error)

	mu     sync.Mutex
	cached string
}

type postHogCapture struct {
	APIKey     string                 `json:"api_key"`
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
		httpClient: &http.Client{
			Timeout: 750 * time.Millisecond,
		},
		now:     time.Now,
		homeDir: os.UserHomeDir,
	}
}

func analyticsDisabled() bool {
	value := strings.TrimSpace(os.Getenv(envDisabled))
	return value == "1" || strings.EqualFold(value, "true") || strings.EqualFold(value, "yes")
}

func (c *Client) TrackCommand(command, output string, verbose bool, runErr error, duration time.Duration) {
	if c == nil {
		return
	}
	anonID, err := c.anonymousID()
	if err != nil {
		return
	}
	event := postHogCapture{
		APIKey:     c.apiKey,
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
	if err := c.appendEvent(event); err != nil {
		return
	}
	_ = c.send(event)
}

func (c *Client) appendEvent(event postHogCapture) error {
	path, err := c.eventsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(payload, '\n'))
	return err
}

func (c *Client) send(event postHogCapture) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("posthog endpoint returned %s", resp.Status)
	}
	return nil
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

func (c *Client) eventsPath() (string, error) {
	home, err := c.homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, analyticsDirName, eventsFile), nil
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
