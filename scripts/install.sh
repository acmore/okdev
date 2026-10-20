#!/usr/bin/env sh
set -eu

REPO="${OKDEV_REPO:-acmore/okdev}"
VERSION="${1:-latest}"

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in
  x86_64) arch="amd64" ;;
  aarch64|arm64) arch="arm64" ;;
  *)
    echo "unsupported architecture: $arch" >&2
    exit 1
    ;;
esac

if [ "$os" != "linux" ] && [ "$os" != "darwin" ]; then
  echo "unsupported OS: $os" >&2
  exit 1
fi

artifact="okdev_${os}_${arch}.tar.gz"
if [ "$VERSION" = "latest" ]; then
  base="https://github.com/${REPO}/releases/latest/download"
else
  base="https://github.com/${REPO}/releases/download/${VERSION}"
fi
url="${base}/${artifact}"

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT INT TERM
archive="$tmpdir/$artifact"

echo "Downloading $url"
curl -fsSL "$url" -o "$archive"
curl -fsSL "${base}/checksums.txt" -o "$tmpdir/checksums.txt"

expected="$(awk -v f="$artifact" '$2 == f { print $1 }' "$tmpdir/checksums.txt")"
if [ -z "$expected" ]; then
  echo "no checksum published for $artifact" >&2
  exit 1
fi
if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "$archive" | awk '{print $1}')"
else
  actual="$(shasum -a 256 "$archive" | awk '{print $1}')"
fi
if [ "$actual" != "$expected" ]; then
  echo "checksum mismatch for $artifact: expected $expected, got $actual" >&2
  exit 1
fi
echo "Checksum verified"

# The checksum only proves the archive matches a file published next to it.
# The build provenance attestation proves okdev's release workflow built it.
# A release published before attestations has none: that is the same
# checksum-only trust as a machine without the GitHub CLI. An attestation
# that exists but does not verify aborts the install.
if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
  if verify_out="$(gh attestation verify "$archive" --repo "$REPO" \
    --signer-workflow "${REPO}/.github/workflows/release.yml" 2>&1)"; then
    echo "Build provenance verified"
  elif printf '%s' "$verify_out" | grep -q "HTTP 404"; then
    echo "Warning: this release has no build provenance attestation; installed on checksum alone"
  else
    echo "build provenance verification failed:" >&2
    printf '%s\n' "$verify_out" >&2
    exit 1
  fi
else
  echo "Note: install and log in to the GitHub CLI to also verify build provenance:"
  echo "  gh attestation verify <archive> --repo $REPO --signer-workflow ${REPO}/.github/workflows/release.yml"
fi

tar -xzf "$archive" -C "$tmpdir"

target_dir="${OKDEV_INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$target_dir"
install "$tmpdir/okdev" "$target_dir/okdev"

echo "Installed okdev to $target_dir/okdev"
echo "Add to PATH if needed: export PATH=\"$target_dir:\$PATH\""
