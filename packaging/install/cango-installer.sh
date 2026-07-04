#!/bin/sh
# cango installer — downloads the prebuilt codeanalyzer-go (`cango`) binary for your
# platform from the GitHub Release and installs it. Mirrors the cargo-dist installer pattern.
#
# Usage:
#   curl --proto '=https' --tlsv1.2 -LsSf https://github.com/codellm-devkit/codeanalyzer-go/releases/latest/download/cango-installer.sh | sh
#
# Environment overrides:
#   CANGO_INSTALL_DIR   install location           (default: ~/.local/bin)
#   CANGO_VERSION       release tag, e.g. v0.3.0    (default: latest)
set -eu

REPO="codellm-devkit/codeanalyzer-go"
INSTALL_DIR="${CANGO_INSTALL_DIR:-$HOME/.local/bin}"
VERSION="${CANGO_VERSION:-latest}"

os="$(uname -s)"
arch="$(uname -m)"

# Map the host platform to the published Release asset name (see packaging/python/build_wheels.sh
# targets and packaging/homebrew/generate_formula.sh).
case "$os" in
  Darwin)
    case "$arch" in
      arm64 | aarch64) asset="cango-macosx_11_0_arm64" ;;
      x86_64) asset="cango-macosx_10_12_x86_64" ;;
      *) echo "cango: unsupported macOS architecture: $arch" >&2; exit 1 ;;
    esac
    ;;
  Linux)
    case "$arch" in
      x86_64) asset="cango-manylinux2014_x86_64" ;;
      aarch64 | arm64) asset="cango-manylinux2014_aarch64" ;;
      *) echo "cango: unsupported Linux architecture: $arch" >&2; exit 1 ;;
    esac
    ;;
  *)
    echo "cango: unsupported OS '$os'. Try: pip install codeanalyzer-go" >&2
    exit 1
    ;;
esac

if [ "$VERSION" = "latest" ]; then
  url="https://github.com/$REPO/releases/latest/download/$asset"
else
  url="https://github.com/$REPO/releases/download/$VERSION/$asset"
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "cango: downloading $asset ($VERSION)..."
if command -v curl >/dev/null 2>&1; then
  curl --proto '=https' --tlsv1.2 -fLsS "$url" -o "$tmp/cango"
elif command -v wget >/dev/null 2>&1; then
  wget -q "$url" -O "$tmp/cango"
else
  echo "cango: need curl or wget to download" >&2
  exit 1
fi

chmod +x "$tmp/cango"
mkdir -p "$INSTALL_DIR"
mv "$tmp/cango" "$INSTALL_DIR/cango"
# Backwards-compatible alias so tools that call `codeanalyzer-go` (e.g. the CLDK
# Python SDK) keep working after a shell-script install.
ln -sf "$INSTALL_DIR/cango" "$INSTALL_DIR/codeanalyzer-go"
echo "cango: installed to $INSTALL_DIR/cango (alias: codeanalyzer-go)"

# PATH hint when the install dir isn't already on PATH.
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) echo "cango: add it to your PATH:  export PATH=\"$INSTALL_DIR:\$PATH\"" ;;
esac
