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
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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
		if !ShouldUpgrade(currentVersion, latest) {
			return
		}
		fmt.Fprintf(w, "\nA new version of okdev is available (%s → %s). Run \"okdev upgrade\" to update.\n", currentVersion, latest)
	})
}

func ShouldUpgrade(currentVersion, latestVersion string) bool {
	current, okCurrent := parseSemver(currentVersion)
	latest, okLatest := parseSemver(latestVersion)
	if okCurrent && okLatest {
		return compareSemver(latest, current) > 0
	}
	currentTrimmed := strings.TrimSpace(strings.TrimPrefix(currentVersion, "v"))
	latestTrimmed := strings.TrimSpace(strings.TrimPrefix(latestVersion, "v"))
	return currentTrimmed != "" && latestTrimmed != "" && currentTrimmed != latestTrimmed
}

type Upgrader struct {
	assetBaseURL string
	httpClient   *http.Client
	goos         string
	goarch       string
	binPath      string
}

func NewUpgrader(version string) (*Upgrader, error) {
	binPath, err := resolveBinaryPath(os.Executable, exec.LookPath, os.Lstat, filepath.EvalSymlinks, os.Args)
	if err != nil {
		return nil, fmt.Errorf("resolve upgrade binary path: %w", err)
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

type semverVersion struct {
	major      int
	minor      int
	patch      int
	prerelease string
}

func parseSemver(v string) (semverVersion, bool) {
	trimmed := strings.TrimSpace(strings.TrimPrefix(v, "v"))
	if trimmed == "" {
		return semverVersion{}, false
	}
	if base, _, ok := strings.Cut(trimmed, "+"); ok {
		trimmed = base
	}
	core := trimmed
	prerelease := ""
	if base, pre, ok := strings.Cut(trimmed, "-"); ok {
		core = base
		prerelease = pre
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return semverVersion{}, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return semverVersion{}, false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return semverVersion{}, false
	}
	patch, err := strconv.Atoi(parts[2])
	if err != nil {
		return semverVersion{}, false
	}
	return semverVersion{major: major, minor: minor, patch: patch, prerelease: prerelease}, true
}

func compareSemver(a, b semverVersion) int {
	switch {
	case a.major != b.major:
		return cmpInt(a.major, b.major)
	case a.minor != b.minor:
		return cmpInt(a.minor, b.minor)
	case a.patch != b.patch:
		return cmpInt(a.patch, b.patch)
	default:
		return comparePrerelease(a.prerelease, b.prerelease)
	}
}

func comparePrerelease(a, b string) int {
	switch {
	case a == "" && b == "":
		return 0
	case a == "":
		return 1
	case b == "":
		return -1
	}
	aParts := strings.Split(a, ".")
	bParts := strings.Split(b, ".")
	for i := 0; i < len(aParts) && i < len(bParts); i++ {
		if aParts[i] == bParts[i] {
			continue
		}
		aNum, aNumOK := parseNumericIdentifier(aParts[i])
		bNum, bNumOK := parseNumericIdentifier(bParts[i])
		switch {
		case aNumOK && bNumOK:
			return cmpInt(aNum, bNum)
		case aNumOK:
			return -1
		case bNumOK:
			return 1
		case aParts[i] < bParts[i]:
			return -1
		default:
			return 1
		}
	}
	return cmpInt(len(aParts), len(bParts))
}

func parseNumericIdentifier(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func resolveBinaryPath(
	executable func() (string, error),
	lookPath func(string) (string, error),
	lstat func(string) (fs.FileInfo, error),
	evalSymlinks func(string) (string, error),
	args []string,
) (string, error) {
	binPath, err := executable()
	if err != nil {
		return "", err
	}
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		return binPath, nil
	}
	invokedPath := strings.TrimSpace(args[0])
	if !strings.ContainsRune(invokedPath, filepath.Separator) {
		lookedUp, lookErr := lookPath(invokedPath)
		if lookErr != nil {
			return binPath, nil
		}
		invokedPath = lookedUp
	}
	if !filepath.IsAbs(invokedPath) {
		invokedAbs, err := filepath.Abs(invokedPath)
		if err != nil {
			return "", err
		}
		invokedPath = invokedAbs
	}
	resolvedInvoked, err := evalSymlinks(invokedPath)
	if err != nil {
		return "", err
	}
	resolvedBinary, err := evalSymlinks(binPath)
	if err != nil {
		return "", err
	}
	if resolvedInvoked != resolvedBinary {
		return binPath, nil
	}
	info, err := lstat(invokedPath)
	if err != nil {
		return "", err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return "", fmt.Errorf("binary is invoked through symlink %q; upgrade the target install directly", invokedPath)
	}
	return invokedPath, nil
}
