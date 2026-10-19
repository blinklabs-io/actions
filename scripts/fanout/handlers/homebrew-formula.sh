#!/usr/bin/env bash
# Move a Homebrew formula to the released tag: source url, sha256 and the
# CommitHash stamped into the version package. Needs PRODUCT_REPO
# (owner/name). FANOUT_TARBALL_SHA256 and FANOUT_COMMIT override the network
# lookups.
set -euo pipefail
# shellcheck source=../lib.sh disable=SC1091
source "$(dirname "$0")/../lib.sh"

dir=$1 version=$(fanout_normalize "$3") spec=$4
formula="$dir/$(jq -er '.formula' <<<"$spec")"
repo=${PRODUCT_REPO:?PRODUCT_REPO is required}
[[ -f "$formula" ]] || { echo "ERROR: $formula not found" >&2; exit 1; }

current=$(sed -nE 's#^  url ".*/refs/tags/v?([0-9][^"]*)\.tar\.gz"$#\1#p' "$formula" | head -1)
if [[ -z "$current" ]]; then
  echo "ERROR: no release tarball url in $formula" >&2
  exit 1
fi
fanout_gt "$version" "$current" || exit "$EXIT_NOOP"

tag="v$version"
url="https://github.com/$repo/archive/refs/tags/$tag.tar.gz"
# Tag tarballs are byte-stable, so the sha256 pins the exact release.
sha=${FANOUT_TARBALL_SHA256:-$(curl -fsSL "$url" | sha256sum | cut -d' ' -f1)}
commit=${FANOUT_COMMIT:-$(gh api "repos/$repo/commits/$tag" --jq .sha)}
short=${commit:0:7}
if [[ ! "$sha" =~ ^[0-9a-f]{64}$ || ! "$short" =~ ^[0-9a-f]{7}$ ]]; then
  echo "ERROR: bad release facts for $tag: sha256=$sha commit=$commit" >&2
  exit 1
fi

tmp="$formula.tmp"
sed -E \
  -e "s#^  url \".*\"#  url \"$url\"#" \
  -e "s#^  sha256 \".*\"#  sha256 \"$sha\"#" \
  -e "s#(version\.CommitHash=)[0-9a-f]+#\1$short#" \
  "$formula" > "$tmp"
mv "$tmp" "$formula"
# sed exits 0 when a pattern matches nothing; a renamed field must fail loudly.
for expected in "$url" "$sha" "version.CommitHash=$short"; do
  grep -qF -- "$expected" "$formula" || { echo "ERROR: formula field not updated: $expected" >&2; exit 1; }
done
echo "homebrew-formula: $current -> $version" >&2
