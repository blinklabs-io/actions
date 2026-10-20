#!/usr/bin/env bash
# Apply one release to one target. Exit 0: changed. Exit 10: already current.
# Usage: apply.sh TYPE TARGET_DIR PRODUCT VERSION SPEC_JSON
set -euo pipefail

if [[ $# -ne 5 ]]; then
  echo "usage: $0 TYPE TARGET_DIR PRODUCT VERSION SPEC_JSON" >&2
  exit 2
fi
here=$(cd "$(dirname "$0")" && pwd)
case "$1" in
  ansible-role | helm-chart | cardano-up-package | compose-default | homebrew-formula | version-refs) ;;
  *) echo "ERROR: unknown fan-out type: $1" >&2; exit 2 ;;
esac
exec "$here/handlers/$1.sh" "$2" "$3" "$4" "$5"
