package analytics

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestAnonymousIDPersists(t *testing.T) {
	tmp := t.TempDir()
	client := &Client{
		httpClient: &http.Client{Timeout: time.Second},
		now:        func() time.Time { return time.Unix(1700000000, 0) },
		homeDir:    func() (string, error) { return tmp, nil },
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

func TestTrackCommandPostsPostHogCaptureAndWritesJournal(t *testing.T) {
	tmp := t.TempDir()
	var got postHogCapture
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		defer close(done)
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	client := &Client{
		endpoint:   server.URL,
		apiKey:     "phc_test_key",
		httpClient: server.Client(),
		now:        func() time.Time { return time.Unix(1700000000, 0).UTC() },
		homeDir:    func() (string, error) { return tmp, nil },
	}

	client.TrackCommand("okdev up", "json", true, nil, 1250*time.Millisecond)
	<-done

	if got.APIKey != "phc_test_key" {
		t.Fatalf("unexpected api key %q", got.APIKey)
	}
	if got.Event != "okdev command completed" {
		t.Fatalf("unexpected event name %q", got.Event)
	}
	if got.Properties["command"] != "okdev up" {
		t.Fatalf("unexpected command property %#v", got.Properties["command"])
	}
	if got.Properties["success"] != true {
		t.Fatalf("unexpected success property %#v", got.Properties["success"])
	}
	if got.Properties["duration_ms"] != float64(1250) {
		t.Fatalf("unexpected duration %#v", got.Properties["duration_ms"])
	}
	if got.Properties["output"] != "json" {
		t.Fatalf("unexpected output property %#v", got.Properties["output"])
	}

	journalPath := filepath.Join(tmp, analyticsDirName, eventsFile)
	data, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatalf("read events journal: %v", err)
	}
	if !strings.Contains(string(data), "\"event\":\"okdev command completed\"") {
		t.Fatalf("expected journal to contain PostHog event, got %q", string(data))
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

func boolPtr(v bool) *bool { return &v }
