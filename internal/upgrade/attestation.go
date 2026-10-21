package upgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/golang/snappy"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// Release archives are verified against the GitHub build-provenance
// attestation the release workflow publishes (Sigstore keyless signing).
// The checksum file alone only proves the archive matches a file published
// next to it; the attestation proves this repository's release workflow,
// running for this tag, produced exactly these bytes.
const (
	releaseRepo         = "acmore/okdev"
	githubActionsIssuer = "https://token.actions.githubusercontent.com"
	releaseWorkflow     = ".github/workflows/release.yml"
	maxAttestationBytes = 16 << 20
)

// ErrNoAttestation means GitHub has no attestation for the archive digest:
// a release published before attestations, or one whose attestation step
// did not run.
var ErrNoAttestation = errors.New("no build provenance attestation published for this release archive")

// releaseIdentity is the only signer accepted for a release: the release
// workflow of this repository, running for exactly this tag.
func releaseIdentity(version string) (verify.CertificateIdentity, error) {
	san := fmt.Sprintf("https://github.com/%s/%s@refs/tags/%s", releaseRepo, releaseWorkflow, version)
	return verify.NewShortCertificateIdentity(githubActionsIssuer, "", san, "")
}

// fetchAttestationBundles returns the raw Sigstore bundles GitHub stores for
// a sha256 digest. Bundles come inline or behind a (snappy-compressed)
// bundle_url, depending on the API.
func fetchAttestationBundles(ctx context.Context, client *http.Client, apiBase, repo, digestHex string) ([][]byte, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/attestations/sha256:%s", strings.TrimRight(apiBase, "/"), repo, digestHex)
	body, status, err := httpGetLimited(ctx, client, endpoint)
	if err != nil {
		return nil, fmt.Errorf("fetch attestations: %w", err)
	}
	if status == http.StatusNotFound {
		return nil, ErrNoAttestation
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("fetch attestations: HTTP %d", status)
	}
	var resp struct {
		Attestations []struct {
			Bundle    json.RawMessage `json:"bundle"`
			BundleURL string          `json:"bundle_url"`
		} `json:"attestations"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode attestations: %w", err)
	}
	var bundles [][]byte
	for _, att := range resp.Attestations {
		if len(att.Bundle) > 0 && string(att.Bundle) != "null" {
			bundles = append(bundles, att.Bundle)
			continue
		}
		if att.BundleURL == "" {
			continue
		}
		raw, status, err := httpGetLimited(ctx, client, att.BundleURL)
		if err != nil || status != http.StatusOK {
			continue
		}
		if u, parseErr := url.Parse(att.BundleURL); parseErr == nil && strings.HasSuffix(u.Path, ".sn") {
			if raw, err = snappy.Decode(nil, raw); err != nil {
				continue
			}
		}
		bundles = append(bundles, raw)
	}
	if len(bundles) == 0 {
		return nil, ErrNoAttestation
	}
	return bundles, nil
}

func httpGetLimited(ctx context.Context, client *http.Client, rawURL string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAttestationBytes))
	return body, resp.StatusCode, err
}

// verifyAttestationBundles accepts the artifact when any bundle carries a
// valid Sigstore signature over the digest from the expected identity.
// Others (such as GitHub's own release attestations, signed by a different
// Sigstore instance) are ignored rather than rejected.
func verifyAttestationBundles(trusted root.TrustedMaterial, bundles [][]byte, digest []byte, identity verify.CertificateIdentity) error {
	verifier, err := verify.NewVerifier(trusted,
		verify.WithSignedCertificateTimestamps(1),
		verify.WithTransparencyLog(1),
		verify.WithObserverTimestamps(1))
	if err != nil {
		return err
	}
	policy := verify.NewPolicy(verify.WithArtifactDigest("sha256", digest), verify.WithCertificateIdentity(identity))
	var errs []error
	for _, raw := range bundles {
		var b bundle.Bundle
		if err := b.UnmarshalJSON(raw); err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := verifier.Verify(&b, policy); err != nil {
			errs = append(errs, err)
			continue
		}
		return nil
	}
	return fmt.Errorf("no attestation verified for the expected release workflow: %w", errors.Join(errs...))
}

// publicGoodTrustedRoot fetches the Sigstore public-good trusted root over
// TUF (rooted in the TUF root embedded in sigstore-go), cached under
// ~/.okdev so repeat upgrades work from the cache.
func publicGoodTrustedRoot() (root.TrustedMaterial, error) {
	opts := tuf.DefaultOptions()
	if home, err := os.UserHomeDir(); err == nil {
		opts = opts.WithCachePath(filepath.Join(home, ".okdev", "sigstore-tuf"))
	}
	client, err := tuf.New(opts)
	if err != nil {
		return nil, fmt.Errorf("initialize Sigstore TUF client: %w", err)
	}
	data, err := client.GetTarget("trusted_root.json")
	if err != nil {
		return nil, fmt.Errorf("fetch Sigstore trusted root: %w", err)
	}
	return root.NewTrustedRootFromJSON(bytes.TrimSpace(data))
}
