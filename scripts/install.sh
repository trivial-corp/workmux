#!/bin/sh
# Install the latest workmux release.
#
#   curl -fsSL https://raw.githubusercontent.com/trivial-corp/workmux/main/scripts/install.sh | sh
#
# Honest about what it does: downloads one static binary for your platform from
# GitHub Releases, verifies it against the published checksums, and puts it in
# ~/.local/bin (or $WORKMUX_BIN_DIR). No daemon, no sudo, nothing else touched.
set -eu

REPO=trivial-corp/workmux
BIN_DIR="${WORKMUX_BIN_DIR:-$HOME/.local/bin}"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) echo "workmux: no build for $arch" >&2; exit 1 ;;
esac
case "$os" in
  darwin | linux) ;;
  *) echo "workmux: no build for $os" >&2; exit 1 ;;
esac

version="${WORKMUX_VERSION:-}"
if [ -z "$version" ]; then
  version=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" |
            sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
fi
[ -n "$version" ] || { echo "workmux: could not find the latest release" >&2; exit 1; }

num=${version#v}
archive="workmux_${num}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$version"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
echo "workmux $version ($os/$arch)"
curl -fsSL "$base/$archive" -o "$tmp/$archive"

# Verify before running anything from it.
if curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt" 2>/dev/null; then
  want=$(grep " $archive\$" "$tmp/checksums.txt" | awk '{print $1}')
  if [ -n "$want" ]; then
    if command -v shasum >/dev/null; then got=$(shasum -a 256 "$tmp/$archive" | awk '{print $1}')
    elif command -v sha256sum >/dev/null; then got=$(sha256sum "$tmp/$archive" | awk '{print $1}')
    else got=""; fi
    if [ -n "$got" ] && [ "$got" != "$want" ]; then
      echo "workmux: checksum mismatch — refusing to install" >&2
      exit 1
    fi
  fi
fi

tar -xzf "$tmp/$archive" -C "$tmp"
mkdir -p "$BIN_DIR"
mv "$tmp/workmux" "$BIN_DIR/workmux"
chmod +x "$BIN_DIR/workmux"

echo "installed $BIN_DIR/workmux"
case ":$PATH:" in
  *":$BIN_DIR:"*) echo "run it in any git repo:  workmux" ;;
  *) echo "add it to your PATH:  export PATH=\"$BIN_DIR:\$PATH\"" ;;
esac
