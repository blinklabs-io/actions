#!/usr/bin/env bash
# shellcheck shell=bash
# Shared helpers for the release fan-out handlers. Source this file; it does not
# change shell options.

FANOUT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# Handlers exit with this code when the target is already current.
# shellcheck disable=SC2034 # consumed by the sourcing handlers
EXIT_NOOP=10
# fanout_refs returns this when refs is non-empty but no glob matched a file.
# shellcheck disable=SC2034 # consumed by fanout_refs callers
EXIT_NOMATCH=3
# fanout_refs returns this when refs.pl failed on a file, so a crash is never
# mistaken for "unchanged" and a partial update is never reported as success.
# shellcheck disable=SC2034 # consumed by fanout_refs callers
EXIT_REFS_FAILED=4

# fanout_normalize TAG: strip the leading "v" and a numeric image revision
# suffix ("v11.1.3-1" -> "11.1.3"). Accepts X.Y.Z, X.Y.Z.W and YYYYMMDD only.
fanout_normalize() {
  local v="${1#v}"
  if [[ ! "$v" =~ ^([0-9]+(\.[0-9]+){2,3}|[0-9]{8})(-[0-9]+)?$ ]]; then
    echo "ERROR: not a release version: $1" >&2
    return 1
  fi
  printf '%s\n' "${BASH_REMATCH[1]}"
}

# fanout_gt A B: success when A is strictly newer than B.
fanout_gt() {
  [[ "$1" != "$2" && "$(printf '%s\n%s\n' "$1" "$2" | sort -V | tail -1)" == "$1" ]]
}

# fanout_refs DIR PRODUCT VERSION SPEC_JSON: rewrite version references in the
# files matched by spec.refs (globs relative to DIR). Success when any file
# changed; status 1 when nothing did; status EXIT_NOMATCH when refs is
# non-empty but no glob matched any file, so a renamed file fails loudly; and
# status EXIT_REFS_FAILED when refs.pl failed on any file, even after an earlier
# file changed.
fanout_refs() {
  local dir=$1 product=$2 version=$3 spec=$4 changed=1 matched=0 declared=0 glob file rc
  while IFS= read -r glob; do
    [[ -n "$glob" ]] || continue
    declared=$((declared + 1))
    while IFS= read -r file; do
      matched=$((matched + 1))
      rc=0
      perl "$FANOUT_DIR/refs.pl" "$product" "$version" "$dir/$file" || rc=$?
      case "$rc" in
        0) changed=0 ;;
        1) ;;
        *)
          echo "ERROR: refs.pl failed on $file (status $rc)" >&2
          return "$EXIT_REFS_FAILED"
          ;;
      esac
    done < <(
      cd "$dir" || exit 1
      shopt -s globstar nullglob
      # shellcheck disable=SC2086 # the glob must expand
      for f in $glob; do [[ -f "$f" ]] && printf '%s\n' "$f"; done
    )
  done < <(jq -r '.refs // [] | .[]' <<<"$spec")
  if [[ "$declared" -gt 0 && "$matched" -eq 0 ]]; then
    echo "ERROR: refs matched no files: $(jq -r '.refs | join(", ")' <<<"$spec")" >&2
    return "$EXIT_NOMATCH"
  fi
  return "$changed"
}

# fanout_commit_msg PRODUCT VERSION SELF: the commit subject. A commit to the
# product's own repository carries [skip ci] so it does not rebuild images.
fanout_commit_msg() {
  local msg="chore($1): update to v$2"
  if [[ "$3" == "true" ]]; then msg="$msg [skip ci]"; fi
  printf '%s\n' "$msg"
}
