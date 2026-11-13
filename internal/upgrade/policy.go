package upgrade

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	policyAssetName = "release-policy.json"
	policyCacheFile = "release_policy.json"
	policyCacheTTL  = time.Hour
	maxPolicyBytes  = 8 << 10
)

// ReleasePolicy is published and attested with each release. The floor is a
// repository-wide setting, not a project config field that callers can edit.
type ReleasePolicy struct {
	SchemaVersion     int       `json:"schemaVersion"`
	MinimumCLIVersion string    `json:"minimumCliVersion"`
	EnforceAfter      time.Time `json:"enforceAfter"`
}

type policyCacheEntry struct {
	Policy    ReleasePolicy `json:"policy"`
	LatestTag string        `json:"latestTag"`
	CheckedAt time.Time     `json:"checkedAt"`
}

type policyReleaseResponse struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

type PolicyChecker struct {
	apiBase    string
	httpClient *http.Client
	homeDir    func() (string, error)
	now        func() time.Time
	verify     func(context.Context, string, []byte) error
}

func NewPolicyChecker() *PolicyChecker {
	c := &PolicyChecker{
		apiBase:    defaultAPIBase,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		homeDir:    os.UserHomeDir,
		now:        time.Now,
	}
	c.verify = c.verifyPolicy
	return c
}

// Check allows the command only after a recent, verified policy permits this
// CLI version. Network or verification failures are errors, not permission.
func (c *PolicyChecker) Check(ctx context.Context, currentVersion string) error {
	current, ok := parseSemver(currentVersion)
	if !ok {
		return fmt.Errorf("check minimum CLI version: invalid current version %q", currentVersion)
	}
	entry, err := c.load(ctx, currentVersion)
	if err != nil {
		return fmt.Errorf("check minimum CLI version: %w; run \"okdev upgrade\" when network access is available", err)
	}
	minimum, _ := parseSemver(entry.Policy.MinimumCLIVersion)
	if !c.now().Before(entry.Policy.EnforceAfter) && compareSemver(current, minimum) < 0 {
		return fmt.Errorf("okdev %s is below the minimum supported version %s (latest: %s); run \"okdev upgrade\"", currentVersion, entry.Policy.MinimumCLIVersion, entry.LatestTag)
	}
	return nil
}

func (c *PolicyChecker) load(ctx context.Context, currentVersion string) (policyCacheEntry, error) {
	previous, previousErr := c.readCache()
	if previousErr == nil {
		age := c.now().Sub(previous.CheckedAt)
		if age >= 0 && age < policyCacheTTL {
			return previous, nil
		}
	}
	entry, err := c.fetch(ctx, currentVersion)
	if err != nil {
		return policyCacheEntry{}, err
	}
	if previousErr == nil {
		oldMinimum, _ := parseSemver(previous.Policy.MinimumCLIVersion)
		newMinimum, _ := parseSemver(entry.Policy.MinimumCLIVersion)
		oldTag, _ := parseSemver(previous.LatestTag)
		newTag, _ := parseSemver(entry.LatestTag)
		if compareSemver(newMinimum, oldMinimum) < 0 || compareSemver(newTag, oldTag) < 0 {
			return policyCacheEntry{}, fmt.Errorf("release policy rollback detected")
		}
	}
	entry.CheckedAt = c.now()
	if err := c.writeCache(entry); err != nil {
		return policyCacheEntry{}, fmt.Errorf("cache verified release policy: %w", err)
	}
	return entry, nil
}

func (c *PolicyChecker) fetch(ctx context.Context, currentVersion string) (policyCacheEntry, error) {
	body, status, err := httpGetLimited(ctx, c.httpClient, strings.TrimRight(c.apiBase, "/")+repoPath)
	if err != nil {
		return policyCacheEntry{}, err
	}
	if status != http.StatusOK {
		return policyCacheEntry{}, fmt.Errorf("GitHub release API returned HTTP %d", status)
	}
	var release policyReleaseResponse
	if err := json.Unmarshal(body, &release); err != nil {
		return policyCacheEntry{}, fmt.Errorf("decode latest release: %w", err)
	}
	latest, ok := parseSemver(release.TagName)
	if !ok || !strings.HasPrefix(release.TagName, "v") {
		return policyCacheEntry{}, fmt.Errorf("invalid latest release tag %q", release.TagName)
	}
	var assetURL string
	for _, asset := range release.Assets {
		if asset.Name == policyAssetName {
			assetURL = asset.URL
			break
		}
	}
	current, _ := parseSemver(currentVersion)
	if assetURL == "" && current.prerelease != "" {
		// Before the first stable release with a policy, a prerelease must
		// read its own attested asset; GitHub's latest endpoint omits it.
		body, status, err = httpGetLimited(ctx, c.httpClient, strings.TrimRight(c.apiBase, "/")+"/repos/"+releaseRepo+"/releases/tags/"+url.PathEscape(currentVersion))
		if err != nil {
			return policyCacheEntry{}, err
		}
		if status != http.StatusOK {
			return policyCacheEntry{}, fmt.Errorf("GitHub prerelease API returned HTTP %d", status)
		}
		release = policyReleaseResponse{}
		if err := json.Unmarshal(body, &release); err != nil || release.TagName != currentVersion {
			return policyCacheEntry{}, fmt.Errorf("invalid prerelease response for %s", currentVersion)
		}
		latest, _ = parseSemver(release.TagName)
		for _, asset := range release.Assets {
			if asset.Name == policyAssetName {
				assetURL = asset.URL
				break
			}
		}
	}
	if assetURL == "" {
		return policyCacheEntry{}, fmt.Errorf("latest release %s has no %s", release.TagName, policyAssetName)
	}
	data, status, err := httpGetLimited(ctx, c.httpClient, assetURL)
	if err != nil {
		return policyCacheEntry{}, err
	}
	if status != http.StatusOK || len(data) > maxPolicyBytes {
		return policyCacheEntry{}, fmt.Errorf("fetch release policy: HTTP %d or policy too large", status)
	}
	digest := sha256.Sum256(data)
	if err := c.verify(ctx, release.TagName, digest[:]); err != nil {
		return policyCacheEntry{}, fmt.Errorf("verify release policy attestation: %w", err)
	}
	var policy ReleasePolicy
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		return policyCacheEntry{}, fmt.Errorf("decode release policy: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return policyCacheEntry{}, fmt.Errorf("release policy has trailing data")
	}
	minimum, ok := validPolicyVersion(policy.MinimumCLIVersion)
	if policy.SchemaVersion != 1 || !ok || policy.EnforceAfter.IsZero() || compareSemver(minimum, latest) > 0 {
		return policyCacheEntry{}, fmt.Errorf("invalid release policy for %s", release.TagName)
	}
	return policyCacheEntry{Policy: policy, LatestTag: release.TagName}, nil
}

func (c *PolicyChecker) verifyPolicy(ctx context.Context, tag string, digest []byte) error {
	bundles, err := fetchAttestationBundles(ctx, c.httpClient, c.apiBase, releaseRepo, hex.EncodeToString(digest))
	if err != nil {
		return err
	}
	trusted, err := publicGoodTrustedRoot()
	if err != nil {
		return err
	}
	identity, err := releaseIdentity(tag)
	if err != nil {
		return err
	}
	return verifyAttestationBundles(trusted, bundles, digest, identity)
}

func (c *PolicyChecker) cachePath() (string, error) {
	home, err := c.homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, cacheDirName, policyCacheFile), nil
}

func (c *PolicyChecker) readCache() (policyCacheEntry, error) {
	path, err := c.cachePath()
	if err != nil {
		return policyCacheEntry{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return policyCacheEntry{}, err
	}
	var entry policyCacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return policyCacheEntry{}, err
	}
	minimum, okMinimum := validPolicyVersion(entry.Policy.MinimumCLIVersion)
	latest, okLatest := parseSemver(entry.LatestTag)
	if entry.Policy.SchemaVersion != 1 || !okMinimum || !okLatest || entry.Policy.EnforceAfter.IsZero() || entry.CheckedAt.IsZero() || compareSemver(minimum, latest) > 0 {
		return policyCacheEntry{}, fmt.Errorf("invalid cached release policy")
	}
	return entry, nil
}

func validPolicyVersion(raw string) (semverVersion, bool) {
	v, ok := parseSemver(raw)
	return v, ok && strings.HasPrefix(raw, "v") && v.major >= 0 && v.minor >= 0 && v.patch >= 0 && v.prerelease == "" && !strings.Contains(raw, "+")
}

func (c *PolicyChecker) writeCache(entry policyCacheEntry) error {
	path, err := c.cachePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".release-policy-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
