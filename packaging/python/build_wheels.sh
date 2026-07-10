#!/usr/bin/env bash
#
# Build platform-tagged Python wheels for the cango (codeanalyzer-go) binary.
#
# For each target: cross-compile the binary with `go build` (GOOS/GOARCH), build a
# (pure) wheel with hatchling, then retag it from `py3-none-any` to the matching
# platform tag with `wheel tags`. The binary is python-agnostic, so each platform
# needs exactly one wheel (py3-none-<platform>), not one per Python version.
#
# Go cross-compiles every target from a single host with no C toolchain
# (CGO_ENABLED=0), so one Linux job suffices — the same shape as the Bun-based
# codeanalyzer-typescript build.
#
# Requirements on the build host:
#   - go 1.25+       (https://go.dev/dl)  -- cross-compiles all targets from one host
#   - python -m pip install build wheel hatchling twine
#     (hatchling is the build backend; --no-isolation means it must be installed)
#
# Usage:
#   ./build_wheels.sh           # build all targets into ./dist
#   twine upload dist/*.whl     # publish
#
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$HERE/../.." && pwd)"          # codeanalyzer-go repo root (has go.mod)
# Version comes from the environment (the release workflow sets it from the git
# tag); the literal is only a local-dev fallback. It is written into __init__.py
# (hatch's single source of truth for the wheel version) and injected into the
# binary via -ldflags so `cango --version` matches the wheel version.
PKG_VERSION="${PKG_VERSION:-0.1.0}"
WHEEL_STEM="codeanalyzer_go-${PKG_VERSION}-py3-none-any.whl"
BIN_DIR="$HERE/src/codeanalyzer_go/_bin"
INIT_PY="$HERE/src/codeanalyzer_go/__init__.py"

# Remove built binaries from _bin/ but keep the tracked .gitignore (and the dir),
# so a local build leaves the working tree pristine.
clean_bin() { mkdir -p "$BIN_DIR"; find "$BIN_DIR" -mindepth 1 ! -name '.gitignore' -delete; }

# Stamp $PKG_VERSION into __init__.py for the build, restoring the original on
# exit so the working tree stays pristine (mirrors the _bin cleanup below).
ORIG_INIT="$(cat "$INIT_PY")"   # $(...) strips the trailing newline; restore re-adds it
restore_init() { printf '%s\n' "$ORIG_INIT" > "$INIT_PY"; }
trap restore_init EXIT
python - "$INIT_PY" "$PKG_VERSION" <<'PY'
import re, sys
path, version = sys.argv[1], sys.argv[2]
text = open(path).read()
new, n = re.subn(r'__version__ = "[^"]*"', f'__version__ = "{version}"', text)
if n != 1:
    raise SystemExit(f"expected exactly one __version__ assignment in {path}, found {n}")
open(path, "w").write(new)
print(f">>> stamped __version__ = {version}")
PY

# "GOOS/GOARCH" : "wheel platform tag"
TARGETS=(
  "darwin/arm64:macosx_11_0_arm64"
  "darwin/amd64:macosx_10_12_x86_64"
  "linux/amd64:manylinux2014_x86_64"
  "linux/arm64:manylinux2014_aarch64"
  "windows/amd64:win_amd64"
)

rm -rf "$HERE/dist"
mkdir -p "$HERE/dist"

# The wheel's long description (the PyPI page) is the repo root README — copy it in so there is a
# single source of truth. It is gitignored and removed on exit (see cleanup) to keep the tree pristine.
cp "$REPO_ROOT/README.md" "$HERE/README.md"

for entry in "${TARGETS[@]}"; do
  goplat="${entry%%:*}"           # e.g. darwin/arm64
  plat="${entry##*:}"             # e.g. macosx_11_0_arm64
  goos="${goplat%%/*}"
  goarch="${goplat##*/}"
  ext=""
  [[ "$goos" == "windows" ]] && ext=".exe"

  echo ">>> [$goplat] compiling -> wheel ($plat)"

  clean_bin

  # CGO_ENABLED=0 produces a fully static binary with no libc dependency, so the
  # Linux wheels satisfy the manylinux2014 ABI baseline. -s -w strips the symbol
  # table / DWARF to shrink the binary; -X injects the release version.
  ( cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
      go build -trimpath \
        -ldflags "-s -w -X main.version=${PKG_VERSION}" \
        -o "$BIN_DIR/cango$ext" ./cmd/codeanalyzer )

  # Build a pure wheel (py3-none-any), then retag to the platform.
  python -m build --wheel --no-isolation -o "$HERE/dist" "$HERE"
  python -m wheel tags --remove --platform-tag "$plat" "$HERE/dist/$WHEEL_STEM"
done

# Clean the working binary + copied README so the tree stays pristine.
clean_bin
rm -f "$HERE/README.md"

echo
echo ">>> Built wheels:"
ls -lh "$HERE/dist"/*.whl
echo
echo "Publish with:  twine upload $HERE/dist/*.whl"
