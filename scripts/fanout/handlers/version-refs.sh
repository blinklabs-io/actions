#!/usr/bin/env bash
# Rewrite version references in the files matched by spec.refs.
set -euo pipefail
# shellcheck source=../lib.sh disable=SC1091
source "$(dirname "$0")/../lib.sh"

dir=$1 product=$2 version=$(fanout_normalize "$3") spec=$4
rc=0
fanout_refs "$dir" "$product" "$version" "$spec" || rc=$?
case "$rc" in
  0) exit 0 ;;
  1) exit "$EXIT_NOOP" ;;
  *) exit 1 ;;
esac
