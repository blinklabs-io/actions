#!/usr/bin/env bash
# Shell tests for the release fan-out scripts. Run: bash scripts/fanout/test.sh
# shellcheck disable=SC2016,SC1003 # backticks and a trailing backslash are literal fixture text
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh disable=SC1091
source "$HERE/lib.sh"

fails=0
pass() { echo "ok   - $1"; }
fail() { echo "FAIL - $1"; fails=$((fails + 1)); }
assert_eq() { if [[ "$2" == "$3" ]]; then pass "$1"; else fail "$1: got [$2] want [$3]"; fi; }
assert_contains() { if grep -qF -- "$3" <<<"$2"; then pass "$1"; else fail "$1: missing [$3]"; fi; }
assert_not_contains() { if grep -qF -- "$3" <<<"$2"; then fail "$1: unexpected [$3]"; else pass "$1"; fi; }
# run <name> <cmd...> -> sets RC without tripping set -e
run() { set +e; "${@:2}" >"$TMP/out" 2>"$TMP/err"; RC=$?; set -e; }

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

tree_sum() { (cd "$1" && find . -type f -exec sha256sum {} + | sort); }

fresh() { # copy fixtures into a clean work dir
  rm -rf "$TMP/work"; mkdir -p "$TMP/work"; cp -r "$HERE/testdata/." "$TMP/work/"
}

# --- semver helpers ---------------------------------------------------------
assert_eq "normalize strips v" "$(fanout_normalize v0.70.0)" "0.70.0"
run x fanout_normalize v0.70.0-rc1; assert_eq "normalize rejects prerelease" "$RC" "1"
assert_eq "normalize strips image revision" "$(fanout_normalize v11.1.3-1)" "11.1.3"
assert_eq "normalize keeps four parts" "$(fanout_normalize v11.2.3.1-1)" "11.2.3.1"
assert_eq "normalize accepts date versions" "$(fanout_normalize v20260915-2)" "20260915"
run x fanout_normalize v1.2.3-pre-1; assert_eq "normalize rejects pre suffix" "$RC" "1"
run x fanout_gt 20260915 20260707;  assert_eq "gt date versions" "$RC" "0"
run x fanout_gt 11.2.3.1 11.0.0.0;  assert_eq "gt four parts" "$RC" "0"
run x fanout_normalize latest;      assert_eq "normalize rejects latest" "$RC" "1"
run x fanout_gt 0.70.0 0.69.0;      assert_eq "gt newer" "$RC" "0"
run x fanout_gt 0.69.0 0.69.0;      assert_eq "gt equal is false" "$RC" "1"
run x fanout_gt 0.9.0 0.69.0;       assert_eq "gt compares numerically" "$RC" "1"
run x fanout_gt 1.0.0 0.69.9;       assert_eq "gt major" "$RC" "0"

# --- commit message ---------------------------------------------------------
assert_eq "commit msg" "$(fanout_commit_msg dingo 0.70.0 false)" "chore(dingo): update to v0.70.0"
assert_eq "commit msg self skips ci" "$(fanout_commit_msg dingo 0.70.0 true)" "chore(dingo): update to v0.70.0 [skip ci]"

# --- refs.pl ----------------------------------------------------------------
fresh
run x perl "$HERE/refs.pl" dingo 0.70.0 "$TMP/work/docs/README.md"; assert_eq "refs readme changed" "$RC" "0"
readme=$(cat "$TMP/work/docs/README.md")
assert_contains     "image tag rewritten"   "$readme" 'ghcr.io/blinklabs-io/dingo:0.70.0'
assert_contains     "v-prefix preserved"    "$readme" 'blinklabs/dingo:v0.70.0'
# shellcheck disable=SC2016 # backticks are literal
assert_contains     "prose pin rewritten"   "$readme" 'pin Dingo `0.70.0`'
assert_contains     "other product image untouched" "$readme" 'ghcr.io/blinklabs-io/adder:0.40.0'
assert_contains     "go version untouched"  "$readme" 'Go 1.26.0'
assert_contains     "unanchored 0.68.0 untouched" "$readme" 'Against 0.68.0'

run x perl "$HERE/refs.pl" dingo 0.70.0 "$TMP/work/docs/values.yaml"; assert_eq "refs values changed" "$RC" "0"
values=$(cat "$TMP/work/docs/values.yaml")
assert_contains "dingo tag rewritten" "$values" 'tag: "0.70.0"'
assert_contains "adder tag untouched" "$values" 'tag: "0.40.0"'

run x perl "$HERE/refs.pl" dingo 0.70.0 "$TMP/work/docs/README.md"; assert_eq "refs idempotent" "$RC" "1"

fresh
run x perl "$HERE/refs.pl" dingo 0.68.0 "$TMP/work/docs/README.md"; assert_eq "refs never downgrade (readme)" "$RC" "1"
run x perl "$HERE/refs.pl" dingo 0.68.0 "$TMP/work/docs/values.yaml"; assert_eq "refs never downgrade (values)" "$RC" "1"
assert_contains "readme unchanged after refused downgrade" "$(cat "$TMP/work/docs/README.md")" 'dingo:0.69.0'

# --- fanout_refs globbing ---------------------------------------------------
fresh
run x fanout_refs "$TMP/work" dingo 0.70.0 '{"refs":["docs/*.md","docs/*.yaml","missing/**/*.md"]}'
assert_eq "fanout_refs changed" "$RC" "0"
assert_contains "glob md rewritten"   "$(cat "$TMP/work/docs/README.md")"   'dingo:0.70.0'
assert_contains "glob yaml rewritten" "$(cat "$TMP/work/docs/values.yaml")" 'tag: "0.70.0"'
run x fanout_refs "$TMP/work" dingo 0.70.0 '{"refs":["docs/*.md"]}'
assert_eq "fanout_refs idempotent" "$RC" "1"
run x fanout_refs "$TMP/work" dingo 0.70.0 '{}'
assert_eq "fanout_refs without refs is a no-op" "$RC" "1"

# --- repository lines with trailing comments ---------------------------------
fresh
cat >"$TMP/work/docs/commented.yaml" <<'YAML'
image:
  repository: ghcr.io/blinklabs-io/dingo # main
  tag: "0.69.0"
sidecar:
  image:
    repository: ghcr.io/blinklabs-io/adder # x
    tag: "0.40.0"
YAML
run x perl "$HERE/refs.pl" dingo 0.70.0 "$TMP/work/docs/commented.yaml"; assert_eq "commented repo refs changed" "$RC" "0"
commented=$(cat "$TMP/work/docs/commented.yaml")
assert_contains     "commented dingo repo tag rewritten" "$commented" 'tag: "0.70.0"'
assert_contains     "commented adder repo tag untouched" "$commented" 'tag: "0.40.0"'

cat >"$TMP/work/docs/interleaved.yaml" <<'YAML'
image:
  repository: ghcr.io/blinklabs-io/dingo
  tag: "0.69.0"
sidecar:
  image:
    repository: ghcr.io/blinklabs-io/adder # x
    tag: "0.40.0"
YAML
run x perl "$HERE/refs.pl" dingo 0.70.0 "$TMP/work/docs/interleaved.yaml"; assert_eq "interleaved refs changed" "$RC" "0"
interleaved=$(cat "$TMP/work/docs/interleaved.yaml")
assert_contains     "interleaved dingo tag rewritten" "$interleaved" 'tag: "0.70.0"'
assert_contains     "interleaved adder tag untouched" "$interleaved" 'tag: "0.40.0"'

# A tag after the image block's mapping closed, or nested deeper, is not the image tag.
cat >"$TMP/work/docs/scoped.yaml" <<'YAML'
image:
  repository: ghcr.io/blinklabs-io/dingo
  # comment at the same level

  tag: "0.69.0"
  extra:
    tag: "0.50.0"
other:
  tag: "0.10.0"
nested:
  image:
    repository: ghcr.io/blinklabs-io/dingo
    tag: 0.69.0
top:
  tag: "0.20.0"
YAML
run x perl "$HERE/refs.pl" dingo 0.70.0 "$TMP/work/docs/scoped.yaml"; assert_eq "scoped refs changed" "$RC" "0"
scoped=$(cat "$TMP/work/docs/scoped.yaml")
assert_contains     "sibling tag rewritten" "$scoped" '  tag: "0.70.0"'
assert_contains     "nested image tag rewritten" "$scoped" '    tag: 0.70.0'
assert_contains     "deeper tag untouched" "$scoped" '    tag: "0.50.0"'
assert_contains     "later unrelated tag untouched" "$scoped" '  tag: "0.10.0"'
assert_contains     "later top-level tag untouched" "$scoped" '  tag: "0.20.0"'

# Handler tests are appended by later tasks below this line.

# --- apply.sh dispatcher ----------------------------------------------------
run x "$HERE/apply.sh" bogus "$TMP" dingo 0.70.0 '{}'
assert_eq "unknown type is an error" "$RC" "2"

# --- ansible-role -----------------------------------------------------------
fresh
spec='{"repo":"o/ansible-cardano","type":"ansible-role","role":"dingo"}'
run x "$HERE/apply.sh" ansible-role "$TMP/work/ansible" dingo 0.70.0 "$spec"
assert_eq "ansible upgrade exit" "$RC" "0"
assert_contains "ansible version bumped" "$(cat "$TMP/work/ansible/roles/dingo/defaults/main.yml")" "dingo_version: '0.70.0'"
run x "$HERE/apply.sh" ansible-role "$TMP/work/ansible" dingo 0.70.0 "$spec"
assert_eq "ansible re-run is no-op" "$RC" "10"
run x "$HERE/apply.sh" ansible-role "$TMP/work/ansible" dingo 0.69.0 "$spec"
assert_eq "ansible refuses downgrade" "$RC" "10"
assert_contains "ansible kept 0.70.0" "$(cat "$TMP/work/ansible/roles/dingo/defaults/main.yml")" "dingo_version: '0.70.0'"
run x "$HERE/apply.sh" ansible-role "$TMP/work/ansible" dingo 0.71.0-rc1 "$spec"
assert_eq "ansible rejects prerelease" "$RC" "1"

# --- helm-chart -------------------------------------------------------------
fresh
spec='{"repo":"o/helm-charts","type":"helm-chart","chart":"charts/dingo"}'
run x "$HERE/apply.sh" helm-chart "$TMP/work/helm" dingo 0.70.0 "$spec"
assert_eq "helm upgrade exit" "$RC" "0"
chart=$(cat "$TMP/work/helm/charts/dingo/Chart.yaml"); vals=$(cat "$TMP/work/helm/charts/dingo/values.yaml")
assert_contains "helm appVersion"        "$chart" 'appVersion: "0.70.0"'
assert_contains "helm chart patch bump"  "$chart" 'version: 0.1.14'
assert_contains "helm image tag"         "$vals"  'tag: "0.70.0"'
run x "$HERE/apply.sh" helm-chart "$TMP/work/helm" dingo 0.70.0 "$spec"
assert_eq "helm re-run is no-op" "$RC" "10"
assert_contains "helm chart not bumped twice" "$(cat "$TMP/work/helm/charts/dingo/Chart.yaml")" 'version: 0.1.14'
before=$(tree_sum "$TMP/work/helm")
run x "$HERE/apply.sh" helm-chart "$TMP/work/helm" dingo 0.69.0 "$spec"
assert_eq "helm refuses downgrade" "$RC" "10"
assert_eq "helm downgrade leaves files unchanged" "$(tree_sum "$TMP/work/helm")" "$before"

# --- compose-default --------------------------------------------------------
fresh
mkdir -p "$TMP/work/compose"
printf 'services:\n  a:\n    image: ghcr.io/blinklabs-io/tx-submit-api:${TX_SUBMIT_API_VERSION:-0.22.1}\n  b:\n    image: x:${OTHER_VERSION:-1.0.0}\n' > "$TMP/work/compose/docker-compose.yaml"
kspec='{"repo":"o/stacks","type":"compose-default","file":"docker-compose.yaml","var":"TX_SUBMIT_API_VERSION"}'
run x "$HERE/apply.sh" compose-default "$TMP/work/compose" tx-submit-api 0.23.0 "$kspec"
assert_eq "compose default exit" "$RC" "0"
assert_contains "compose default raised" "$(cat "$TMP/work/compose/docker-compose.yaml")" 'TX_SUBMIT_API_VERSION:-0.23.0}'
assert_contains "compose other var untouched" "$(cat "$TMP/work/compose/docker-compose.yaml")" 'OTHER_VERSION:-1.0.0}'
run x "$HERE/apply.sh" compose-default "$TMP/work/compose" tx-submit-api 0.23.0 "$kspec"
assert_eq "compose default re-run is no-op" "$RC" "10"
run x "$HERE/apply.sh" compose-default "$TMP/work/compose" tx-submit-api 0.20.0 "$kspec"
assert_eq "compose default refuses older" "$RC" "10"
run x "$HERE/apply.sh" compose-default "$TMP/work/compose" tx-submit-api 0.24.0 '{"type":"compose-default","file":"docker-compose.yaml","var":"MISSING"}'
assert_eq "compose default missing var fails" "$RC" "1"

# --- homebrew-formula -------------------------------------------------------
fresh
hspec='{"repo":"o/homebrew-cardano","type":"homebrew-formula","formula":"Formula/dingo.rb"}'
sha=$(printf 'a%.0s' $(seq 64))
export PRODUCT_REPO=o/dingo FANOUT_TARBALL_SHA256=$sha FANOUT_COMMIT=0123456789abcdef0123456789abcdef01234567
run x "$HERE/apply.sh" homebrew-formula "$TMP/work/homebrew" dingo 0.86.0 "$hspec"
assert_eq "formula exit" "$RC" "0"
f=$(cat "$TMP/work/homebrew/Formula/dingo.rb")
assert_contains "formula url"    "$f" 'url "https://github.com/o/dingo/archive/refs/tags/v0.86.0.tar.gz"'
assert_contains "formula sha"    "$f" "sha256 \"$sha\""
assert_contains "formula commit" "$f" 'version.CommitHash=0123456'
run x "$HERE/apply.sh" homebrew-formula "$TMP/work/homebrew" dingo 0.86.0 "$hspec"
assert_eq "formula re-run is no-op" "$RC" "10"
run x "$HERE/apply.sh" homebrew-formula "$TMP/work/homebrew" dingo 0.80.0 "$hspec"
assert_eq "formula refuses older" "$RC" "10"
FANOUT_COMMIT=zz run x "$HERE/apply.sh" homebrew-formula "$TMP/work/homebrew" dingo 0.87.0 "$hspec"
assert_eq "formula rejects bad commit" "$RC" "1"
unset PRODUCT_REPO FANOUT_TARBALL_SHA256 FANOUT_COMMIT

# --- cardano-up-package -----------------------------------------------------
fresh
spec='{"repo":"o/cardano-up-packages","type":"cardano-up-package","package":"dingo"}'
run x "$HERE/apply.sh" cardano-up-package "$TMP/work/packages" dingo 0.70.0 "$spec"
assert_eq "package add exit" "$RC" "0"
newfile="$TMP/work/packages/packages/dingo/dingo-0.70.0.yaml"
if [[ -f "$newfile" ]]; then pass "package file created"; else fail "package file created"; fi
assert_contains "package version field" "$(cat "$newfile")" 'version: 0.70.0'
assert_contains "package image tag"     "$(cat "$newfile")" 'ghcr.io/blinklabs-io/dingo:0.70.0'
fresh
mkdir -p "$TMP/work/packages/packages/cardano-config"
printf 'name: cardano-config\nversion: 20251128\nimage: ghcr.io/blinklabs-io/cardano-configs:20251128-1\n' \
  > "$TMP/work/packages/packages/cardano-config/cardano-config-20251128.yaml"
cspec='{"repo":"o/cardano-up-packages","type":"cardano-up-package","package":"cardano-config"}'
FANOUT_REVISION=3 run x "$HERE/apply.sh" cardano-up-package "$TMP/work/packages" docker-cardano-configs 20260915 "$cspec"
assert_eq "config add exit" "$RC" "0"
assert_contains "config image revision from tag" \
  "$(cat "$TMP/work/packages/packages/cardano-config/cardano-config-20260915.yaml")" 'cardano-configs:20260915-3'
fresh
run x "$HERE/apply.sh" cardano-up-package "$TMP/work/packages" dingo 0.70.0 "$spec"
run x "$HERE/apply.sh" cardano-up-package "$TMP/work/packages" dingo 0.70.0 "$spec"
assert_eq "package re-run is no-op" "$RC" "10"
before=$(tree_sum "$TMP/work/packages")
run x "$HERE/apply.sh" cardano-up-package "$TMP/work/packages" dingo 0.50.0 "$spec"
assert_eq "package refuses older version" "$RC" "10"
assert_eq "package older version leaves dir unchanged" "$(tree_sum "$TMP/work/packages")" "$before"

# --- version-refs -----------------------------------------------------------
fresh
spec='{"repo":"o/dingo","type":"version-refs","refs":["docs/*.md","docs/*.yaml"]}'
run x "$HERE/apply.sh" version-refs "$TMP/work" dingo 0.70.0 "$spec"
assert_eq "refs upgrade exit" "$RC" "0"
run x "$HERE/apply.sh" version-refs "$TMP/work" dingo 0.70.0 "$spec"
assert_eq "refs re-run is no-op" "$RC" "10"

# --- ansible-role also rewrites its README refs ------------------------------
fresh
spec='{"repo":"o/ansible-cardano","type":"ansible-role","role":"dingo","refs":["roles/dingo/README.md"]}'
# shellcheck disable=SC2016 # backticks are literal
printf 'Install the `ghcr.io/blinklabs-io/dingo:0.20.0` image.\n' >> "$TMP/work/ansible/roles/dingo/README.md"
run x "$HERE/apply.sh" ansible-role "$TMP/work/ansible" dingo 0.70.0 "$spec"
assert_contains "ansible README ref rewritten" "$(cat "$TMP/work/ansible/roles/dingo/README.md")" 'dingo:0.70.0'

# --- refs that match nothing -------------------------------------------------
fresh
run x fanout_refs "$TMP/work" dingo 0.70.0 '{"refs":["nope/*.md","gone/**/*.yaml"]}'
assert_eq "fanout_refs all-zero-match is an error" "$RC" "$EXIT_NOMATCH"
assert_contains "fanout_refs names the unmatched globs" "$(cat "$TMP/err")" "nope/*.md, gone/**/*.yaml"
run x fanout_refs "$TMP/work" dingo 0.70.0 '{"refs":["nope/*.md","docs/*.md"]}'
assert_eq "fanout_refs partial match still works" "$RC" "0"
run x fanout_refs "$TMP/work" dingo 0.70.0 '{"refs":[]}'
assert_eq "fanout_refs empty refs is a no-op" "$RC" "1"
spec='{"repo":"o/dingo","type":"version-refs","refs":["missing/*.md"]}'
run x "$HERE/apply.sh" version-refs "$TMP/work" dingo 0.70.0 "$spec"
assert_eq "version-refs zero-match exits 1" "$RC" "1"
spec='{"repo":"o/ansible-cardano","type":"ansible-role","role":"dingo","refs":["missing/*.md"]}'
run x "$HERE/apply.sh" ansible-role "$TMP/work/ansible" dingo 0.71.0 "$spec"
assert_eq "ansible-role zero-match refs exits 1" "$RC" "1"
spec='{"repo":"o/ansible-cardano","type":"ansible-role","role":"dingo"}'
fresh
run x "$HERE/apply.sh" ansible-role "$TMP/work/ansible" dingo 0.71.0 "$spec"
assert_eq "ansible-role without refs unaffected" "$RC" "0"

# --- <product>_version and image.tag rows ---------------------------------------
fresh
mkdir -p "$TMP/work/live"
# shellcheck disable=SC2016 # backticks are literal
cat > "$TMP/work/live/ansible.md" <<'EOF'
| `dingo_version` | `0.83.1` | Image tag; architecture suffixes can be included. |
| `dingo_docker_image` | `ghcr.io/blinklabs-io/dingo:{{ dingo_version }}` | Full image reference. |
      dingo_version: '0.80.0'
| `adder_version` | `0.10.0` | Another product. |
EOF
run x perl "$HERE/refs.pl" dingo 99.0.0 "$TMP/work/live/ansible.md"
assert_eq "dingo_version rewritten" "$RC" "0"
out=$(cat "$TMP/work/live/ansible.md")
assert_contains "table cell updated" "$out" '| `dingo_version` | `99.0.0` | Image tag;'
assert_contains "assignment updated" "$out" "dingo_version: '99.0.0'"
assert_contains "template reference untouched" "$out" '{{ dingo_version }}'
assert_contains "other product untouched" "$out" '| `adder_version` | `0.10.0` |'
printf '| `dingo_version` | `0.90.0` | x |\n' > "$TMP/work/live/down.md"
run x perl "$HERE/refs.pl" dingo 0.83.1 "$TMP/work/live/down.md"
assert_eq "dingo_version never downgrades" "$RC" "1"

# shellcheck disable=SC2016 # backticks are literal
cat > "$TMP/work/live/helm.md" <<'EOF'
docker buildx imagetools inspect ghcr.io/blinklabs-io/dingo:0.83.0 \
`0.83.0-amd64` or `0.83.0-arm64` tag) and verify it before pinning:
| `image.tag`                     | Image tag (used when `image.digest` is empty)             | `0.83.0`                       |
| `image.digest`                  | Image digest                                              | `0.83.0`                       |
EOF
run x perl "$HERE/refs.pl" dingo 99.0.0 "$TMP/work/live/helm.md"
assert_eq "helm README rewritten" "$RC" "0"
out=$(cat "$TMP/work/live/helm.md")
assert_contains "image ref updated" "$out" 'dingo:99.0.0 \'
assert_contains "image.tag row last value updated" "$(grep -F '| `image.tag`' <<<"$out")" '`99.0.0`'
assert_contains "image.tag row keeps its description" "$out" 'Image tag (used when `image.digest` is empty)'
assert_contains "arch suffixed tags untouched" "$out" '`0.83.0-amd64` or `0.83.0-arm64`'
assert_contains "image.digest row untouched" "$out" '| `image.digest`                  | Image digest                                              | `0.83.0`'
printf '| `image.tag` | tag | `0.90.0` |\n' > "$TMP/work/live/down2.md"
run x perl "$HERE/refs.pl" dingo 0.83.0 "$TMP/work/live/down2.md"
assert_eq "image.tag row never downgrades" "$RC" "1"

# --- helm-chart validation before writing -----------------------------------
hspec='{"repo":"o/helm-charts","type":"helm-chart","chart":"charts/dingo"}'
hdir="$TMP/work/helm/charts/dingo"

fresh
sed -i -E 's/^version: .*/version: "0.1.13"/' "$hdir/Chart.yaml"
run x "$HERE/apply.sh" helm-chart "$TMP/work/helm" dingo 0.70.0 "$hspec"
assert_eq "helm quoted chart version exit" "$RC" "0"
assert_contains "helm quoted chart version bumped" "$(cat "$hdir/Chart.yaml")" 'version: "0.1.14"'

fresh
sed -i -E 's/^version: .*/version: banana/' "$hdir/Chart.yaml"
before=$(tree_sum "$TMP/work/helm")
run x "$HERE/apply.sh" helm-chart "$TMP/work/helm" dingo 0.70.0 "$hspec"
assert_eq "helm unparseable chart version is an error" "$RC" "1"
assert_eq "helm unparseable chart version writes nothing" "$(tree_sum "$TMP/work/helm")" "$before"

fresh
sed -i -E 's/^(  tag: ).*/\1""/' "$hdir/values.yaml"
valsum=$(sha256sum <"$hdir/values.yaml")
run x "$HERE/apply.sh" helm-chart "$TMP/work/helm" dingo 0.70.0 "$hspec"
assert_eq "helm empty tag exit" "$RC" "0"
assert_contains "helm empty tag appVersion bumped" "$(cat "$hdir/Chart.yaml")" 'appVersion: "0.70.0"'
assert_contains "helm empty tag chart bumped" "$(cat "$hdir/Chart.yaml")" 'version: 0.1.14'
assert_eq "helm empty tag leaves values unchanged" "$(sha256sum <"$hdir/values.yaml")" "$valsum"

fresh
sed -i -E 's/^(  tag: ).*/\1latest/' "$hdir/values.yaml"
before=$(tree_sum "$TMP/work/helm")
run x "$HERE/apply.sh" helm-chart "$TMP/work/helm" dingo 0.70.0 "$hspec"
assert_eq "helm non-semver tag is an error" "$RC" "1"
assert_eq "helm non-semver tag writes nothing" "$(tree_sum "$TMP/work/helm")" "$before"

# A tag newer than the target stays when appVersion is older.
fresh
sed -i -E 's/^(  tag: ).*/\1"0.75.0"/' "$hdir/values.yaml"
valsum=$(sha256sum <"$hdir/values.yaml")
run x "$HERE/apply.sh" helm-chart "$TMP/work/helm" dingo 0.70.0 "$hspec"
assert_eq "helm newer tag exit" "$RC" "0"
assert_eq "helm newer tag is not downgraded" "$(sha256sum <"$hdir/values.yaml")" "$valsum"
assert_contains "helm newer tag appVersion bumped" "$(cat "$hdir/Chart.yaml")" 'appVersion: "0.70.0"'
assert_contains "helm newer tag chart bumped" "$(cat "$hdir/Chart.yaml")" 'version: 0.1.14'

fresh
sed -i -E 's/^(  tag: ).*/\1"0.70.0"/' "$hdir/values.yaml"
valsum=$(sha256sum <"$hdir/values.yaml")
run x "$HERE/apply.sh" helm-chart "$TMP/work/helm" dingo 0.70.0 "$hspec"
assert_eq "helm equal tag exit" "$RC" "0"
assert_eq "helm equal tag unchanged" "$(sha256sum <"$hdir/values.yaml")" "$valsum"

# --- refs.pl failures are fatal, never "unchanged" ---------------------------
# A perl shim that crashes on one file name and delegates otherwise.
mkdir -p "$TMP/shim"
REAL_PERL=$(command -v perl)
cat >"$TMP/shim/perl" <<SHIM
#!/usr/bin/env bash
if [[ "\${@: -1}" == *values.yaml ]]; then echo "shim: crash" >&2; exit 255; fi
exec "$REAL_PERL" "\$@"
SHIM
chmod +x "$TMP/shim/perl"
REAL_PATH=$PATH
PATH="$TMP/shim:$REAL_PATH"

fresh
run x fanout_refs "$TMP/work" dingo 0.70.0 '{"refs":["docs/values.yaml"]}'
assert_eq "fanout_refs crash on only file" "$RC" "$EXIT_REFS_FAILED"
assert_contains "fanout_refs names the crashed file" "$(cat "$TMP/err")" "refs.pl failed on docs/values.yaml (status 255)"
run x "$HERE/apply.sh" version-refs "$TMP/work" dingo 0.70.0 '{"repo":"o/dingo","type":"version-refs","refs":["docs/values.yaml"]}'
assert_eq "version-refs crash on only file exits 1" "$RC" "1"

fresh
run x "$HERE/apply.sh" version-refs "$TMP/work" dingo 0.70.0 '{"repo":"o/dingo","type":"version-refs","refs":["docs/*.md","docs/*.yaml"]}'
assert_eq "version-refs crash after a changed file exits 1" "$RC" "1"
assert_contains "earlier file did change before the crash" "$(cat "$TMP/work/docs/README.md")" 'dingo:0.70.0'
fresh
run x "$HERE/apply.sh" ansible-role "$TMP/work/ansible" dingo 0.70.0 '{"repo":"o/a","type":"ansible-role","role":"dingo","refs":["../docs/*.md","../docs/*.yaml"]}'
assert_eq "ansible-role crash after a changed file exits 1" "$RC" "1"
fresh
run x "$HERE/apply.sh" helm-chart "$TMP/work/helm" dingo 0.70.0 '{"repo":"o/h","type":"helm-chart","chart":"charts/dingo","refs":["../docs/*.md","../docs/*.yaml"]}'
assert_eq "helm-chart crash after a changed file exits 1" "$RC" "1"
fresh
run x "$HERE/apply.sh" cardano-up-package "$TMP/work/packages" dingo 0.70.0 '{"repo":"o/p","type":"cardano-up-package","package":"dingo","refs":["../docs/*.md","../docs/*.yaml"]}'
assert_eq "cardano-up-package crash after a changed file exits 1" "$RC" "1"

fresh
run x fanout_refs "$TMP/work" dingo 0.70.0 '{"refs":["docs/*.md"]}'
assert_eq "shim delegates for other files (changed)" "$RC" "0"
run x fanout_refs "$TMP/work" dingo 0.70.0 '{"refs":["docs/*.md"]}'
assert_eq "shim delegates for other files (unchanged)" "$RC" "1"
PATH=$REAL_PATH

if [[ $fails -gt 0 ]]; then echo "$fails failure(s)"; exit 1; fi
echo "all tests passed"
