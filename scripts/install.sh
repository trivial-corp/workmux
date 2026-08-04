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

# A private repository's assets aren't readable anonymously, so a token switches this
# to the API, where an asset is fetched by id. Same script either way.
TOKEN="${GITHUB_TOKEN:-${GH_TOKEN:-}}"
api() {
  if [ -n "$TOKEN" ]; then
    curl -fsSL -H "Authorization: Bearer $TOKEN" -H "X-GitHub-Api-Version: 2022-11-28" "$@"
  else
    curl -fsSL "$@"
  fi
}

version="${WORKMUX_VERSION:-}"
if [ -z "$version" ]; then
  version=$(api "https://api.github.com/repos/$REPO/releases/latest" |
            sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
fi
[ -n "$version" ] || { echo "workmux: could not find the latest release" >&2; exit 1; }

num=${version#v}
archive="workmux_${num}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$version"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
echo "workmux $version ($os/$arch)"

# fetch NAME DEST — by URL when public, by asset id when authenticated.
release_json=""
fetch() {
  if [ -z "$TOKEN" ]; then
    curl -fsSL "$base/$1" -o "$2"
    return
  fi
  [ -n "$release_json" ] || release_json=$(api "https://api.github.com/repos/$REPO/releases/tags/$version")
  # The API pretty-prints, so an asset's id and its name are on different lines. Keep
  # the last id seen and print it when the name matches — no jq, which a fresh box
  # won't have.
  id=$(printf '%s\n' "$release_json" | awk -v want="$1" '
    /"id":[[:space:]]*[0-9]+/ { n=$0; gsub(/[^0-9]/, "", n); last=n }
    index($0, "\"name\": \"" want "\"") { print last; exit }')
  [ -n "$id" ] || { echo "workmux: $1 is not in release $version" >&2; return 1; }
  curl -fsSL -H "Authorization: Bearer $TOKEN" -H "Accept: application/octet-stream" \
       "https://api.github.com/repos/$REPO/releases/assets/$id" -o "$2"
}

fetch "$archive" "$tmp/$archive"

# Verify before running anything from it.
if fetch checksums.txt "$tmp/checksums.txt" 2>/dev/null; then
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
