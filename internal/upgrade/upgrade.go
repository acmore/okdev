package upgrade

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultAPIBase = "https://api.github.com"
	repoPath       = "/repos/acmore/okdev/releases/latest"
	cacheDirName   = ".okdev/upgrade"
	cacheFile      = "latest_version"
	cacheTTL       = 24 * time.Hour
)

type Checker struct {
	apiBase    string
	httpClient *http.Client
	homeDir    func() (string, error)
	now        func() time.Time
}

func NewChecker() *Checker {
	return &Checker{
		apiBase:    defaultAPIBase,
		httpClient: &http.Client{Timeout: 5 * time.Second},
		homeDir:    os.UserHomeDir,
		now:        time.Now,
	}
}

type releaseResponse struct {
	TagName string `json:"tag_name"`
}

type cacheEntry struct {
	Version   string    `json:"version"`
	CheckedAt time.Time `json:"checked_at"`
}

func (c *Checker) LatestVersion() (string, error) {
	req, err := http.NewRequest(http.MethodGet, c.apiBase+repoPath, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub API returned %s", resp.Status)
	}
	var rel releaseResponse
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", err
	}
	tag := strings.TrimSpace(rel.TagName)
	if tag == "" {
		return "", fmt.Errorf("empty tag_name in GitHub response")
	}
	return tag, nil
}

func (c *Checker) LatestVersionCached() (string, error) {
	if entry, err := c.readCache(); err == nil {
		if c.now().Sub(entry.CheckedAt) < cacheTTL {
			return entry.Version, nil
		}
	}
	v, err := c.LatestVersion()
	if err != nil {
		return "", err
	}
	_ = c.writeCache(cacheEntry{Version: v, CheckedAt: c.now()})
	return v, nil
}

func (c *Checker) cachePath() (string, error) {
	home, err := c.homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, cacheDirName, cacheFile), nil
}

func (c *Checker) readCache() (cacheEntry, error) {
	path, err := c.cachePath()
	if err != nil {
		return cacheEntry{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cacheEntry{}, err
	}
	var entry cacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return cacheEntry{}, err
	}
	return entry, nil
}

func (c *Checker) writeCache(entry cacheEntry) error {
	path, err := c.cachePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
