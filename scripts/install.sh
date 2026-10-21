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
allow_unattested="${OKDEV_ALLOW_UNATTESTED:-0}"
have_gh=0
if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
  have_gh=1
elif [ "$allow_unattested" != 1 ]; then
  echo "okdev verifies that the release was built by its release workflow, which needs the GitHub CLI (gh)." >&2
  if ! command -v gh >/dev/null 2>&1; then
    echo "gh is not installed. Install it, then log in:" >&2
    if [ "$os" = "darwin" ]; then
      echo "  brew install gh" >&2
    else
      echo "  see https://github.com/cli/cli/blob/trunk/docs/install_linux.md (apt, dnf, ...)" >&2
    fi
  else
    echo "gh is installed but not logged in. Log in:" >&2
  fi
  echo "  gh auth login" >&2
  echo "then re-run this installer." >&2
  echo "To install on the checksum alone instead, set OKDEV_ALLOW_UNATTESTED=1." >&2
  exit 1
fi

if [ "$VERSION" = "latest" ]; then
  if [ "$have_gh" = 1 ]; then
    VERSION="$(gh release view --repo "$REPO" --json tagName --jq '.tagName')"
    if [ -z "$VERSION" ]; then
      echo "could not resolve latest release tag" >&2
      exit 1
    fi
  fi
fi
base="https://github.com/${REPO}/releases/download/${VERSION}"
if [ "$VERSION" = "latest" ]; then
  base="https://github.com/${REPO}/releases/latest/download"
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
# Pin the attestation to this release's tag as well as its signing workflow.
if [ "$have_gh" = 1 ]; then
  if verify_out="$(gh attestation verify "$archive" --repo "$REPO" \
    --signer-workflow "${REPO}/.github/workflows/release.yml" \
    --source-ref "refs/tags/${VERSION}" 2>&1)"; then
    echo "Build provenance verified"
  elif [ "$allow_unattested" = 1 ] && printf '%s' "$verify_out" | grep -q "HTTP 404"; then
    echo "Warning: this release has no build provenance attestation; installing on checksum alone because OKDEV_ALLOW_UNATTESTED=1 was set" >&2
  else
    echo "build provenance verification failed:" >&2
    printf '%s\n' "$verify_out" >&2
    exit 1
  fi
else
  echo "Warning: installing on checksum alone because OKDEV_ALLOW_UNATTESTED=1 was set" >&2
fi

tar -xzf "$archive" -C "$tmpdir"

target_dir="${OKDEV_INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$target_dir"
install "$tmpdir/okdev" "$target_dir/okdev"

echo "Installed okdev to $target_dir/okdev"
echo "Add to PATH if needed: export PATH=\"$target_dir:\$PATH\""
