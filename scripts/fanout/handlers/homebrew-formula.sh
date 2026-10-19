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

# tag_commit REPO TAG: the commit a tag points to, peeling annotated tags.
tag_commit() {
  local type sha
  read -r type sha < <(gh api "repos/$1/git/ref/tags/$2" --jq '[.object.type, .object.sha] | @tsv')
  while [[ "$type" == "tag" ]]; do
    read -r type sha < <(gh api "repos/$1/git/tags/$sha" --jq '[.object.type, .object.sha] | @tsv')
  done
  [[ "$type" == "commit" ]] || { echo "ERROR: $2 does not point to a commit" >&2; return 1; }
  printf '%s\n' "$sha"
}
commit=${FANOUT_COMMIT:-$(tag_commit "$repo" "$tag")}
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
# sed exits 0 when a pattern matches nothing. Check every field before the
# formula is replaced, so a renamed field leaves it untouched and a retry can
# still repair it rather than seeing a half-updated url as already current.
for expected in "$url" "$sha" "version.CommitHash=$short"; do
  if ! grep -qF -- "$expected" "$tmp"; then
    rm -f "$tmp"
    echo "ERROR: formula field not updated: $expected" >&2
    exit 1
  fi
done
mv "$tmp" "$formula"
echo "homebrew-formula: $current -> $version" >&2
