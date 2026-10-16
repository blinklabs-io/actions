#!/usr/bin/env bash
# Bump <role>_version in roles/<role>/defaults/main.yml and rewrite doc refs.
set -euo pipefail
# shellcheck source=../lib.sh disable=SC1091
source "$(dirname "$0")/../lib.sh"

dir=$1 product=$2 version=$(fanout_normalize "$3") spec=$4
role=$(jq -er '.role' <<<"$spec")
file="$dir/roles/$role/defaults/main.yml"
var="${role}_version"

cur=$(sed -nE "s/^${var}: *['\"]?([0-9]+\.[0-9]+\.[0-9]+)['\"]?.*/\1/p" "$file" | head -1)
if [[ -z "$cur" ]]; then
  echo "ERROR: ${var} not found in ${file}" >&2
  exit 1
fi

changed=1
if fanout_gt "$version" "$cur"; then
  sed -i -E "s/^(${var}: *['\"]?)[0-9]+\.[0-9]+\.[0-9]+/\1${version}/" "$file"
  echo "ansible-role: ${var} ${cur} -> ${version}" >&2
  changed=0
fi
rc=0
fanout_refs "$dir" "$product" "$version" "$spec" || rc=$?
case "$rc" in 0) changed=0 ;; 1) ;; *) exit 1 ;; esac
exit $((changed == 0 ? 0 : EXIT_NOOP))
