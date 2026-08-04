#!/bin/sh
# Build the npm packages that carry the binaries.
#
#   make dist && ./scripts/npm-build.sh 0.1.0
#
# Produces npm/build/<pkg>/ for the launcher and one package per platform. Nothing is
# published here — `npm publish` is a separate, deliberate step.
set -eu

VERSION="${1:?usage: npm-build.sh VERSION}"
VERSION="${VERSION#v}"
ROOT=$(cd -- "$(dirname -- "$0")/.." && pwd)
OUT="$ROOT/npm/build"

rm -rf "$OUT"
mkdir -p "$OUT"

# The launcher, with its optional dependencies pinned to this exact version — a
# floating range would let npm pair a launcher with a binary from another release.
mkdir -p "$OUT/workmux"
cp "$ROOT/npm/workmux/cli.js" "$OUT/workmux/cli.js"
cp "$ROOT/README.md" "$OUT/workmux/README.md"
sed "s/\"0\.0\.0\"/\"$VERSION\"/g; s/\"version\": \"$VERSION\"/\"version\": \"$VERSION\"/" \
  "$ROOT/npm/workmux/package.json" > "$OUT/workmux/package.json"

# One package per platform. os/cpu are what make npm install exactly one of them.
for target in darwin-arm64:darwin:arm64 darwin-amd64:darwin:x64 \
              linux-arm64:linux:arm64 linux-amd64:linux:x64; do
  name="workmux-$(echo "$target" | cut -d: -f1)"
  os=$(echo "$target" | cut -d: -f2)
  cpu=$(echo "$target" | cut -d: -f3)
  goos=$os
  goarch=$(echo "$target" | cut -d: -f1 | cut -d- -f2)
  src="$ROOT/dist/workmux-$goos-$goarch"
  [ -f "$src" ] || { echo "missing $src — run make dist first" >&2; exit 1; }

  dir="$OUT/$name"
  mkdir -p "$dir/bin"
  cp "$src" "$dir/bin/workmux-bin"
  chmod +x "$dir/bin/workmux-bin"
  # A tiny module so the launcher can `require(pkg + "/bin/workmux")` and get a path,
  # which works whatever npm did with hoisting.
  cat > "$dir/bin/workmux.js" <<'JS'
module.exports = require("path").join(__dirname, "workmux-bin");
JS
  cat > "$dir/package.json" <<JSON
{
  "name": "$name",
  "version": "$VERSION",
  "description": "workmux binary for $os $cpu",
  "license": "MIT",
  "repository": { "type": "git", "url": "git+https://github.com/trivial-corp/workmux.git" },
  "os": ["$os"],
  "cpu": ["$cpu"],
  "main": "bin/workmux.js",
  "files": ["bin/workmux-bin", "bin/workmux.js"]
}
JSON
  echo "  $name $VERSION ($(du -h "$src" | cut -f1))"
done
echo "npm packages in $OUT — publish with: for d in $OUT/*/; do (cd \$d && npm publish --access public); done"
