#!/usr/bin/env bash
# Add packages/<pkg>/<pkg>-<version>.yaml with the repo's own add-version.sh.
set -euo pipefail
# shellcheck source=../lib.sh disable=SC1091
source "$(dirname "$0")/../lib.sh"

dir=$1 product=$2 version=$(fanout_normalize "$3") spec=$4
pkg=$(jq -er '.package' <<<"$spec")

latest=$(find "$dir/packages/$pkg" -maxdepth 1 -name "${pkg}-*.yaml" -type f \
  | sed -E "s#.*/${pkg}-(.*)\.yaml#\1#" | sort -V | tail -1)
if [[ -z "$latest" ]]; then
  echo "ERROR: no ${pkg} version files under $dir/packages/$pkg" >&2
  exit 1
fi

changed=1
if fanout_gt "$version" "$latest"; then
  (cd "$dir" && PACKAGE_IMAGE_REVISION="${FANOUT_REVISION:-}" ./scripts/add-version.sh "$pkg" "$version" >&2)
  echo "cardano-up-package: ${pkg} ${latest} -> ${version}" >&2
  changed=0
fi
rc=0
fanout_refs "$dir" "$product" "$version" "$spec" || rc=$?
case "$rc" in 0) changed=0 ;; 1) ;; *) exit 1 ;; esac
exit $((changed == 0 ? 0 : EXIT_NOOP))
