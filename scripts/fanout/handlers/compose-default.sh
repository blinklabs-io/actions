#!/usr/bin/env bash
# Raise the ${VAR:-X} default in a compose file to the released version.
set -euo pipefail
# shellcheck source=../lib.sh disable=SC1091
source "$(dirname "$0")/../lib.sh"

dir=$1 product=$2 version=$(fanout_normalize "$3") spec=$4
file="$dir/$(jq -er '.file' <<<"$spec")"
var=$(jq -er '.var' <<<"$spec")
if [[ ! "$var" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]]; then
  echo "ERROR: invalid variable name: $var" >&2
  exit 1
fi
[[ -f "$file" ]] || { echo "ERROR: $file not found" >&2; exit 1; }

current=$(grep -oE "\\$\\{${var}:-[^}]+\\}" "$file" | head -1 | sed -E 's/.*:-([^}]+)\}/\1/' || true)
if [[ -z "$current" ]]; then
  echo "ERROR: no \${${var}:-...} default in $file" >&2
  exit 1
fi
fanout_gt "$version" "$current" || exit "$EXIT_NOOP"
sed -i -E "s/(\\$\\{${var}:-)[^}]+\\}/\\1${version}}/g" "$file"
echo "compose-default: ${var} ${current} -> ${version} ($product)" >&2
