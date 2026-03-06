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
  url="https://github.com/${REPO}/releases/latest/download/${artifact}"
else
  url="https://github.com/${REPO}/releases/download/${VERSION}/${artifact}"
fi

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT INT TERM
archive="$tmpdir/$artifact"

echo "Downloading $url"
curl -fsSL "$url" -o "$archive"
tar -xzf "$archive" -C "$tmpdir"

target_dir="${OKDEV_INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$target_dir"
install "$tmpdir/okdev" "$target_dir/okdev"

echo "Installed okdev to $target_dir/okdev"
echo "Add to PATH if needed: export PATH=\"$target_dir:\$PATH\""
