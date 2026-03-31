package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
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
	launch     func(func())
}

func NewChecker() *Checker {
	return &Checker{
		apiBase:    defaultAPIBase,
		httpClient: &http.Client{Timeout: 5 * time.Second},
		homeDir:    os.UserHomeDir,
		now:        time.Now,
		launch: func(fn func()) {
			go fn()
		},
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

// CheckAndRemind runs a non-blocking cached version check and prints
// a reminder to w if a newer version is available.
func (c *Checker) CheckAndRemind(currentVersion string, w io.Writer) {
	launch := c.launch
	if launch == nil {
		launch = func(fn func()) {
			go fn()
		}
	}
	launch(func() {
		latest, err := c.LatestVersionCached()
		if err != nil {
			return
		}
		current := strings.TrimPrefix(currentVersion, "v")
		remote := strings.TrimPrefix(latest, "v")
		if current == remote || current == "" || remote == "" {
			return
		}
		fmt.Fprintf(w, "\nA new version of okdev is available (%s → %s). Run \"okdev upgrade\" to update.\n", currentVersion, latest)
	})
}

type Upgrader struct {
	assetBaseURL string
	httpClient   *http.Client
	goos         string
	goarch       string
	binPath      string
}

func NewUpgrader(version string) (*Upgrader, error) {
	binPath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve executable path: %w", err)
	}
	binPath, err = filepath.EvalSymlinks(binPath)
	if err != nil {
		return nil, fmt.Errorf("resolve symlinks: %w", err)
	}
	return &Upgrader{
		assetBaseURL: fmt.Sprintf("https://github.com/acmore/okdev/releases/download/%s/", version),
		httpClient:   &http.Client{Timeout: 60 * time.Second},
		goos:         runtime.GOOS,
		goarch:       runtime.GOARCH,
		binPath:      binPath,
	}, nil
}

func (u *Upgrader) assetName() string {
	return fmt.Sprintf("okdev_%s_%s.tar.gz", u.goos, u.goarch)
}

func (u *Upgrader) Upgrade() error {
	checksums, err := u.fetchChecksums()
	if err != nil {
		return fmt.Errorf("fetch checksums: %w", err)
	}
	assetName := u.assetName()
	expectedHash, ok := checksums[assetName]
	if !ok {
		return fmt.Errorf("no checksum found for %s", assetName)
	}
	tarGzData, err := u.fetchAsset(assetName)
	if err != nil {
		return fmt.Errorf("download %s: %w", assetName, err)
	}
	actualHash := sha256.Sum256(tarGzData)
	if hex.EncodeToString(actualHash[:]) != expectedHash {
		return fmt.Errorf("checksum mismatch for %s", assetName)
	}
	bin, err := extractBinary(tarGzData)
	if err != nil {
		return fmt.Errorf("extract binary: %w", err)
	}
	return atomicReplace(u.binPath, bin)
}

func (u *Upgrader) fetchChecksums() (map[string]string, error) {
	data, err := u.fetchAsset("checksums.txt")
	if err != nil {
		return nil, err
	}
	result := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.Fields(line)
		if len(parts) == 2 {
			result[parts[1]] = parts[0]
		}
	}
	return result, nil
}

func (u *Upgrader) fetchAsset(name string) ([]byte, error) {
	resp, err := u.httpClient.Get(u.assetBaseURL + name)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	return io.ReadAll(resp.Body)
}

func extractBinary(tarGzData []byte) ([]byte, error) {
	gr, err := gzip.NewReader(bytes.NewReader(tarGzData))
	if err != nil {
		return nil, err
	}
	defer gr.Close()
	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("binary not found in archive")
		}
		if err != nil {
			return nil, err
		}
		if hdr.Name == "okdev" {
			return io.ReadAll(tr)
		}
	}
}

func atomicReplace(path string, data []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, info.Mode()); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
