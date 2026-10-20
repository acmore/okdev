package upgrade

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/golang/snappy"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// Real attestations for cli/cli v2.102.0's gh_2.102.0_linux_amd64.tar.gz,
// verified offline against a recorded Sigstore public-good trusted root:
// one build-provenance bundle (public-good, signed by cli/cli's workflow)
// and one GitHub release attestation (GitHub's own Sigstore instance).
const ghArchiveDigest = "bb766f710eef8ede859c18578c72c327597cd4c8a85b06001b1f3843c6019386"

func loadAttestationFixtures(t *testing.T) (root.TrustedMaterial, [][]byte, []byte) {
	t.Helper()
	rootJSON, err := os.ReadFile("testdata/sigstore-public-good-trusted-root.json")
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := root.NewTrustedRootFromJSON(rootJSON)
	if err != nil {
		t.Fatal(err)
	}
	var bundles [][]byte
	for _, name := range []string{"gh-cli-release-attestation.json", "gh-cli-provenance-attestation.json"} {
		b, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		bundles = append(bundles, b)
	}
	digest, err := hex.DecodeString(ghArchiveDigest)
	if err != nil {
		t.Fatal(err)
	}
	return trusted, bundles, digest
}

func identity(t *testing.T, san string) verify.CertificateIdentity {
	t.Helper()
	id, err := verify.NewShortCertificateIdentity(githubActionsIssuer, "", san, "")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestVerifyAttestationBundlesAcceptsExpectedWorkflow(t *testing.T) {
	trusted, bundles, digest := loadAttestationFixtures(t)
	ghWorkflow := identity(t, "https://github.com/cli/cli/.github/workflows/deployment.yml@refs/heads/trunk")
	if err := verifyAttestationBundles(trusted, bundles, digest, ghWorkflow); err != nil {
		t.Fatalf("genuine provenance rejected: %v", err)
	}
}

func TestVerifyAttestationBundlesRejectsForgeries(t *testing.T) {
	trusted, bundles, digest := loadAttestationFixtures(t)
	ghWorkflow := identity(t, "https://github.com/cli/cli/.github/workflows/deployment.yml@refs/heads/trunk")

	// A genuine attestation from another repository's workflow is not okdev's.
	okdevRelease, err := releaseIdentity("v2.102.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyAttestationBundles(trusted, bundles, digest, okdevRelease); err == nil {
		t.Fatal("attestation from another repository accepted")
	}
	// Tampered archive: the digest no longer matches the signed subject.
	tampered := append([]byte(nil), digest...)
	tampered[0] ^= 0xff
	if err := verifyAttestationBundles(trusted, bundles, tampered, ghWorkflow); err == nil {
		t.Fatal("attestation accepted for a different digest")
	}
	// GitHub's release attestation is signed by a different Sigstore instance
	// and identity; it alone must not satisfy the policy.
	if err := verifyAttestationBundles(trusted, bundles[:1], digest, ghWorkflow); err == nil {
		t.Fatal("non-provenance attestation accepted")
	}
}

func TestReleaseIdentityPinsWorkflowAndTag(t *testing.T) {
	id, err := releaseIdentity("v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if got := id.SubjectAlternativeName.SubjectAlternativeName; got != "https://github.com/acmore/okdev/.github/workflows/release.yml@refs/tags/v1.2.3" {
		t.Fatalf("unexpected SAN %q", got)
	}
	if id.Issuer.Issuer != githubActionsIssuer {
		t.Fatalf("unexpected issuer %q", id.Issuer.Issuer)
	}
}

func TestFetchAttestationBundles(t *testing.T) {
	_, bundles, _ := loadAttestationFixtures(t)
	compressed := snappy.Encode(nil, bundles[1])
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/attestations/sha256:missing"):
			http.NotFound(w, r)
		case strings.Contains(r.URL.Path, "/attestations/sha256:"):
			_, _ = w.Write([]byte(`{"attestations":[{"bundle":` + string(bundles[0]) + `},{"bundle":null,"bundle_url":"` + srv.URL + `/blob/x.json.sn?sig=1"}]}`))
		case r.URL.Path == "/blob/x.json.sn":
			_, _ = w.Write(compressed)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	got, err := fetchAttestationBundles(context.Background(), srv.Client(), srv.URL, "acmore/okdev", "abc")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || string(got[1]) != string(bundles[1]) {
		t.Fatalf("expected inline + decompressed bundle, got %d", len(got))
	}
	if _, err := fetchAttestationBundles(context.Background(), srv.Client(), srv.URL, "acmore/okdev", "missing"); err != ErrNoAttestation {
		t.Fatalf("expected ErrNoAttestation, got %v", err)
	}
}
