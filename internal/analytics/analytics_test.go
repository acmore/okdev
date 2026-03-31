package analytics

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	posthog "github.com/posthog/posthog-go"
	"gopkg.in/yaml.v3"
)

func TestAnonymousIDPersists(t *testing.T) {
	tmp := t.TempDir()
	client := &Client{
		now:     func() time.Time { return time.Unix(1700000000, 0) },
		homeDir: func() (string, error) { return tmp, nil },
	}

	first, err := client.anonymousID()
	if err != nil {
		t.Fatalf("anonymousID: %v", err)
	}
	client.cached = ""
	second, err := client.anonymousID()
	if err != nil {
		t.Fatalf("anonymousID reload: %v", err)
	}
	if first == "" || first != second {
		t.Fatalf("expected persistent anonymous ID, got %q then %q", first, second)
	}
}

type fakePostHogClient struct {
	messages []posthog.Message
	closed   bool
}

func (f *fakePostHogClient) Enqueue(msg posthog.Message) error {
	f.messages = append(f.messages, msg)
	return nil
}

func (f *fakePostHogClient) Close() error {
	f.closed = true
	return nil
}

func (f *fakePostHogClient) CloseWithContext(context.Context) error {
	f.closed = true
	return nil
}

func (f *fakePostHogClient) IsFeatureEnabled(posthog.FeatureFlagPayload) (interface{}, error) {
	return nil, nil
}

func (f *fakePostHogClient) GetFeatureFlag(posthog.FeatureFlagPayload) (interface{}, error) {
	return nil, nil
}

func (f *fakePostHogClient) GetFeatureFlagResult(posthog.FeatureFlagPayload) (*posthog.FeatureFlagResult, error) {
	return nil, nil
}

func (f *fakePostHogClient) GetFeatureFlagPayload(posthog.FeatureFlagPayload) (string, error) {
	return "", nil
}

func (f *fakePostHogClient) GetRemoteConfigPayload(string) (string, error) {
	return "", nil
}

func (f *fakePostHogClient) GetAllFlags(posthog.FeatureFlagPayloadNoKey) (map[string]interface{}, error) {
	return nil, nil
}

func (f *fakePostHogClient) ReloadFeatureFlags() error {
	return nil
}

func (f *fakePostHogClient) GetFeatureFlags() ([]posthog.FeatureFlag, error) {
	return nil, nil
}

func TestTrackCommandPostsPostHogCapture(t *testing.T) {
	tmp := t.TempDir()
	var createdAPIKey string
	var createdEndpoint string
	fake := &fakePostHogClient{}

	client := &Client{
		endpoint: "https://us.i.posthog.com/capture/",
		apiKey:   "phc_test_key",
		now:      func() time.Time { return time.Unix(1700000000, 0).UTC() },
		homeDir:  func() (string, error) { return tmp, nil },
		newPostHogCLI: func(apiKey, endpoint string) (posthog.Client, error) {
			createdAPIKey = apiKey
			createdEndpoint = endpoint
			return fake, nil
		},
	}

	client.TrackCommand("okdev up", "json", true, nil, 1250*time.Millisecond)

	if createdAPIKey != "phc_test_key" {
		t.Fatalf("unexpected api key %q", createdAPIKey)
	}
	if createdEndpoint != "https://us.i.posthog.com/capture/" {
		t.Fatalf("unexpected endpoint %q", createdEndpoint)
	}
	if len(fake.messages) != 1 {
		t.Fatalf("expected 1 enqueued message, got %d", len(fake.messages))
	}
	capture, ok := fake.messages[0].(posthog.Capture)
	if !ok {
		t.Fatalf("expected posthog.Capture, got %T", fake.messages[0])
	}
	if capture.Event != "okdev command completed" {
		t.Fatalf("unexpected event name %q", capture.Event)
	}
	if capture.Properties["command"] != "okdev up" {
		t.Fatalf("unexpected command property %#v", capture.Properties["command"])
	}
	if capture.Properties["success"] != true {
		t.Fatalf("unexpected success property %#v", capture.Properties["success"])
	}
	if capture.Properties["duration_ms"] != int64(1250) {
		t.Fatalf("unexpected duration %#v", capture.Properties["duration_ms"])
	}
	if capture.Properties["output"] != "json" {
		t.Fatalf("unexpected output property %#v", capture.Properties["output"])
	}
	if !fake.closed {
		t.Fatal("expected posthog client to be closed")
	}
}

func TestUserConfigDisablesAnalytics(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	configDir := filepath.Join(home, ".okdev")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := userConfig{Analytics: boolPtr(false)}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	if !userConfigDisabled() {
		t.Fatal("expected analytics to be disabled via user config")
	}
}

func TestUserConfigDefaultAllowsAnalytics(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// No config file at all — should not disable.
	if userConfigDisabled() {
		t.Fatal("expected analytics to be allowed when no config exists")
	}
}

func TestNormalizePostHogEndpoint(t *testing.T) {
	cases := map[string]string{
		"https://us.i.posthog.com/capture/": "https://us.i.posthog.com",
		"https://us.i.posthog.com/capture":  "https://us.i.posthog.com",
		"https://us.i.posthog.com/":         "https://us.i.posthog.com",
		"https://us.i.posthog.com":          "https://us.i.posthog.com",
	}
	for input, want := range cases {
		if got := normalizePostHogEndpoint(input); got != want {
			t.Fatalf("normalizePostHogEndpoint(%q)=%q want %q", input, got, want)
		}
	}
}

func boolPtr(v bool) *bool { return &v }
