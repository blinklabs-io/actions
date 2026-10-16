#!/usr/bin/env bash
# Bump appVersion and image.tag, patch-bump the chart version, rewrite doc refs.
set -euo pipefail
# shellcheck source=../lib.sh disable=SC1091
source "$(dirname "$0")/../lib.sh"

dir=$1 product=$2 version=$(fanout_normalize "$3") spec=$4
chart=$(jq -er '.chart' <<<"$spec")
cdir="$dir/$chart"

# image_tag FILE: the value of the image block's tag, empty when the line is
# absent or has no value.
image_tag() {
  awk '
    /^image:/ { in_image = 1; next }
    in_image && /^[^[:space:]#]/ { in_image = 0 }
    in_image && /^  tag:/ {
      v = $0
      sub(/^  tag:[[:space:]]*/, "", v)
      sub(/[[:space:]]+#.*$/, "", v)
      gsub(/^["\x27]|["\x27]$/, "", v)
      print v
      exit
    }
  ' "$1"
}

cur=$(sed -nE 's/^appVersion: *"?([0-9]+\.[0-9]+\.[0-9]+)"?.*/\1/p' "$cdir/Chart.yaml" | head -1)
if [[ -z "$cur" ]]; then
  echo "ERROR: appVersion not found in $cdir/Chart.yaml" >&2
  exit 1
fi

changed=1
if fanout_gt "$version" "$cur"; then
  chart_ver=$(sed -nE "s/^version: *[\"']?([0-9]+\.[0-9]+\.[0-9]+)[\"']?[[:space:]]*(#.*)?$/\1/p" "$cdir/Chart.yaml" | head -1)
  if [[ -z "$chart_ver" ]]; then
    echo "ERROR: chart version is not X.Y.Z in $cdir/Chart.yaml" >&2
    exit 1
  fi
  tag=$(image_tag "$cdir/values.yaml")
  if [[ -n "$tag" && ! "$tag" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "ERROR: image tag is not a release version in $cdir/values.yaml: $tag" >&2
    exit 1
  fi

  IFS=. read -r maj min pat <<<"$chart_ver"
  new_chart_ver="${maj}.${min}.$((pat + 1))"

  # Edit copies and move them into place only after both verify.
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  cp "$cdir/Chart.yaml" "$tmp/Chart.yaml"
  cp "$cdir/values.yaml" "$tmp/values.yaml"
  sed -i -E "s/^(appVersion: *\"?)[0-9]+\.[0-9]+\.[0-9]+/\1${version}/" "$tmp/Chart.yaml"
  sed -i -E "s/^(version: *[\"']?)[0-9]+\.[0-9]+\.[0-9]+/\1${new_chart_ver}/" "$tmp/Chart.yaml"
  if [[ -n "$tag" ]]; then
    # Only the image block's tag; a later top-level key ends the range.
    sed -i -E '/^image:/,/^[^[:space:]#]/ s/^(  tag: *["'"'"']?)[0-9]+\.[0-9]+\.[0-9]+/\1'"$version"'/' "$tmp/values.yaml"
  fi
  new_tag=$(image_tag "$tmp/values.yaml")
  if [[ -n "$new_tag" && "$new_tag" != "$version" ]]; then
    echo "ERROR: could not rewrite image tag in $cdir/values.yaml" >&2
    exit 1
  fi
  cat "$tmp/Chart.yaml" >"$cdir/Chart.yaml"
  cat "$tmp/values.yaml" >"$cdir/values.yaml"
  echo "helm-chart: appVersion ${cur} -> ${version}, chart ${chart_ver} -> ${new_chart_ver}" >&2
  changed=0
fi
rc=0
fanout_refs "$dir" "$product" "$version" "$spec" || rc=$?
case "$rc" in 0) changed=0 ;; 1) ;; *) exit 1 ;; esac
exit $((changed == 0 ? 0 : EXIT_NOOP))
