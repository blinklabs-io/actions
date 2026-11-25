# Blink Labs Actions

This repository contains the reusable GitHub Actions workflows shared across
Blink Labs repositories, plus a small **Go governance engine** that keeps every
repository aligned to a single declarative source of truth: `repos-config.yaml`.

The engine reads `repos-config.yaml`, and for each active repository it
reconciles repository settings, collaborators, team grants, branch protection, and the
generated workflow wrapper files directly on that repository's default branch.

## Runtime requirements

- Go ≥ 1.26
- [`github.com/google/go-github/v60`](https://github.com/google/go-github) — GitHub REST API client
- [`gopkg.in/yaml.v3`](https://gopkg.in/yaml.v3) — parses `repos-config.yaml`

## Repository layout

```text
app/
  main.go        # Governance engine: reads repos-config.yaml and reconciles each repo
  main_test.go   # Unit tests (config parsing + workflow template rendering)
templates/
  workflow.tmpl  # text/template used to render downstream workflow wrappers
repos-config.yaml  # Declarative source of truth for every managed repository
.github/workflows/ # Central reusable workflows + the sync engine (sync.yaml)
```

## Reusable workflows

Central workflow templates live in `.github/workflows` and are called from
downstream repositories via `workflow_call`. The current set:

- `reuseable-ci-docker-multiarch.yml` — native multi-arch Docker CI build (no QEMU); `architectures` limits it to the architectures an image ships for, `build-args` passes Dockerfile ARGs, and `check-dockerfile` runs `docker build --check` first.
- `reuseable-publish-docker-multiarch.yml` — native multi-arch Docker publish to Docker Hub + GHCR, with build provenance attestations always enabled and Trivy vulnerability scanning enabled by default (override with `enable-trivy-scan: false`).
- `reuseable-publish.yml` — Go release binaries + native multi-arch Docker image publish; supports explicit target pairs, custom build commands, extra binaries, checksums, prereleases, and optional tag-only build preparation shared by binary and image builds. A re-run of a failed tag build replaces its own stale draft release and refuses to touch a published one. `extra-image-target` publishes a second Dockerfile target as `<tag>-<extra-image-suffix>` on both architectures, and `homebrew-tap-repository` opens a formula-bump pull request on the tap after the release is published (skipped when `homebrew-tap-token` is unset).
- `reuseable-publish-desktop.yml` — full release pipeline for desktop-style Go apps that ship a Fyne GUI tray alongside a CLI: macOS signed + notarized `.pkg` installers, Windows signed `.msi` installers (jsign + WiX), Linux/FreeBSD tarballs, native multi-arch Docker images, and build-provenance attestations for every binary, installer, and image. The packaging scripts and `make` targets it drives live in the calling repository. `binary-target-matrix` (a JSON array of `runner`/`os`/`arch`/`tray` rows, defaulting to all eight targets) lets a caller drop a target.
- `reuseable-go-test.yml` — Go module verification, tidy checks, build, vet, tests, in-job golangci-lint and NilAway, Buf checks, generated-code drift checks, forbidden-import guards, nested modules, service containers, additional test passes, and explicit cross-build targets; supports an optional Linux race-detection pass. `test-tags` applies to the build, vet, cross-build and every test pass (including nested modules), `test-ldflags` to every test pass, and `build-flags` to the build and cross-build (`test-ldflags` exists because `test-flags` is split on whitespace and cannot carry a quoted `-ldflags` value), and `timeout-minutes` sets the job backstop.
- `reuseable-golangci-lint.yml` — golangci-lint with a configurable tool version, additional invocation, optional formatting-diff check, nested modules (`additional-working-directories`), and a `pre-lint-command` for repository checks that belong in the lint stage.
- `reuseable-nilaway.yml` — NilAway static analysis.
- `reuseable-govulncheck.yml` — govulncheck, with an optional gosec SARIF upload; `govulncheck-command` substitutes a repository's own scan (for example a Makefile target) for the default `go run` invocation.
- `reuseable-conventional-commits.yml` — conventional-commit message check for all pull request commits.
- `reuseable-check-versions.yml` — check upstream image versions against a docker-compose file and open update PRs.
- `reuseable-check-versions-packages.yml` — check upstream release/image versions against `packages/<pkg>/<pkg>-*.yaml` version files, validate with the cardano-up CLI, and open update PRs (for cardano-up package repositories).
- `reuseable-release-dispatch.yml` — announce a published release to the central `release-fanout.yml` workflow (used by the generated `release-fanout.yml` wrapper).
- `reuseable-package-validate.yml` — validate cardano-up package definitions with the cardano-up CLI (plus optional filename/content version-consistency check).
- `reuseable-helm-publish.yml` — package changed charts under `charts/*` and publish them to GHCR on default-branch pushes.
- `reuseable-helm-deploy.yml` — deploy a local or OCI Helm chart to Kubernetes from a caller workflow.

The Helm publisher compares the push's before and after commits, including
all commits in a multi-commit push. It publishes each changed directory that
still contains `Chart.yaml`, skips deleted charts, and publishes all charts on
an initial branch push. Charts are packaged before any upload, then pushed
serially to `oci://ghcr.io/<owner>/<repository>/charts` with the caller's
`GITHUB_TOKEN`. Chart names and versions come from their metadata; bump the
chart version when changing a chart. Dependencies must already be vendored.
The caller must grant `contents: read` and `packages: write` and pin the call
to a full commit SHA. `helm-version` defaults to `v4.3.1`.

Call the Helm charts publisher on every `main` push. It does its
own Git diff instead of using workflow path filters, whose changed-file list
is limited. A push without chart changes performs no packaging or registry
login. It uses no canceling concurrency group: every push must publish its own
changed charts, including when several pushes arrive close together. Configure
the caller in `repos-config.yaml` only after this workflow lands on `main`,
using that merged commit SHA. A feature-branch SHA is not a durable pin after
a squash merge.

To deploy a chart, configure a protected environment in the caller repository
and invoke the workflow from its default branch. Store the target cluster's
kubeconfig as a secret named `kubeconfig` in that environment. Do not store
`kubeconfig` as a repository or organization secret; the workflow rejects those
credentials and fails if the selected environment has no `kubeconfig` secret.
Declare only `kubeconfig` in the caller's secret mapping. GitHub resolves the
selected environment's secret for the deployment job. The caller supplies that
environment, a release name, chart path or
OCI reference, and namespace; `values-files`
accepts one repository-relative file path per line.
Every chart must set `chart-sha256` to the reviewed SHA-256 of the complete
`.tgz` chart archive. For an OCI chart, also set `chart-version`; the archive
hash differs from the OCI manifest digest printed by `helm pull`.
For a local chart with dependencies, commit `Chart.lock` alongside `Chart.yaml`.
The preparation job runs `helm dependency build` only when the lock exists, then
packages the complete chart. If the lock is missing or an upstream chart is
replaced, the workflow fails before accessing the cluster. Helm's lock records
dependency versions; `chart-sha256` checks the actual package bytes, including
the resolved dependencies. Calculate that hash from the complete archive made
with Helm 4.3.1, review it, and store it in the caller workflow. A job without
cluster credentials pulls or packages the chart, checks its hash, and uses Helm
4.3.1 to lint and render it with the caller's values. The deployment job
verifies the prepared archive again before receiving the `kubeconfig` secret
and running `helm upgrade --install --atomic --wait`.

Keep application credentials out of chart files, dependencies, and
`values-files`. The preparation job uploads the complete chart archive as a
workflow artifact for one day. [Anyone with repository read access can download
it](https://docs.github.com/en/actions/how-tos/manage-workflow-runs/download-workflow-artifacts).
Separate values files are not uploaded unless they are also inside the chart
directory. Committed values remain readable in the repository, and Helm stores
[supplied values with the release in Kubernetes](https://helm.sh/docs/topics/advanced/#storage-backends).

Provision application secrets separately and put only references to those
Kubernetes Secrets in the chart and values files. Keep `kubeconfig` only in the
selected GitHub environment. The workflow cannot reliably identify and remove
arbitrary secret values; removing fields would change what Helm deploys. Helm
errors may include values in workflow logs, and [GitHub warns that secret
redaction is not guaranteed](https://docs.github.com/en/actions/concepts/security/secrets).

```yaml
name: Deploy
on:
  workflow_dispatch:
jobs:
  deploy:
    uses: blinklabs-io/actions/.github/workflows/reuseable-helm-deploy.yml@<full-commit-sha>
    with:
      release: example
      chart: charts/example
      chart-sha256: <reviewed-sha256-of-packaged-chart>
      namespace: example
      environment: production
      values-files: deploy/production-values.yaml
    secrets:
      kubeconfig: ${{ secrets.kubeconfig }}
```

Replace `<full-commit-sha>` with a reviewed `actions` commit that contains
`reuseable-helm-deploy.yml`.

The shared Go lint workflows default to golangci-lint v2.15.0. Update both
workflow defaults together in a reviewed pull request when advancing the tool.
Callers may override the version explicitly. Shared Linux jobs that install
system packages default to Ubuntu 26.04; package versions remain unpinned.
When a caller selects lint or NilAway by platform, include that exact runner
label in its `platforms` list. A selector absent from the list fails the job
instead of silently skipping the check.

Enable `check-format: "true"` on a repository's Go workflow in
`repos-config.yaml` to enforce gofumpt centrally. The shared formatting step
explicitly enables gofumpt, so repositories do not need a `.golangci.yml` just
to select it. Existing formatter settings and generated-code exclusions are
preserved. In `reuseable-go-test.yml`, also enable `run-golangci-lint`; the
formatting step uses the same tool, version, and platform selectors as lint.
The switch defaults to false so repositories can be enabled individually.

The org's free plan shares 20 concurrent jobs (5 of them macOS) across every repository, so each reusable workflow keeps one run narrow by default:

- `reuseable-go-test.yml` starts cross-builds only after tests pass and builds every `cross-build-targets` entry in a single job; `cross-build-max-parallel` is accepted but ignored. `max-parallel` (default 4) bounds the go-version x go-arch x platform test matrix.
- `reuseable-publish.yml` (`max-parallel`, default 4) bounds the binary build matrix, and `reuseable-go-library-release.yml` (`max-parallel`, default 4) the example build matrix.
- `reuseable-publish-desktop.yml` (`max-parallel`, default 2) bounds the native build matrix, whose two default macOS rows sit at opposite ends so they do not hold two macOS slots at once.

## How the engine works

The engine (`app/main.go`) runs as a batch job — there is no long-running
server or webhook listener. For each active repository in `repos-config.yaml` it
performs an observe → compare → act loop:

1. `syncBranchProtection` — reconciles configured branch protection rules.
2. `reconcileRepoSettings` — reconciles repository settings, including default-branch selection.
3. `reconcileCollaborators` — reconciles collaborators and their permission levels.
4. `reconcileTeams` — reconciles configured team grants after repository controls succeed.
5. `syncWorkflows` — renders standalone wrappers from `templates/workflow.tmpl`
   and grouped pipelines from `templates/pipeline.tmpl`. Configured workflows
   can replace old wrappers in the same commit. Files that already match are
   skipped.

Writes go **directly to each repository's default branch**; the engine does not
open pull requests.

The governance sync reports observed differences before applying changes, using
`-` for the observed value and `+` for the configured value. Results appear in
the job log and GitHub Actions job summary. Public repository file changes are
represented by hashes rather than file contents; collaborator, team, and bypass actor
names are redacted. Team entries use their configuration ordinal. Private or unknown-visibility repositories appear only in
aggregate counts. The report covers configured settings, collaborators, teams, branch
protections, and managed files; it does not audit unmanaged items or identify
who made a change.

Set `sync_paused: true` on a repository to suspend governance during a migration.
Its configuration still participates in rendering and discovery precedence. Sync skips
creation, settings, access controls, and managed files, and disarms archival. Remove
the flag to resume reconciliation against the current configuration.

The final report distinguishes matches, unmanaged settings, corrections,
invitations, failures, unchecked items, and skipped or pending work. A
correction means GitHub accepted the update; collaborator and team permissions are also
read again, while other corrections have no separate verification read. The
collaborator comparison uses effective permissions, which can include grants from
teams or the organization. Membership is observed from the complete collaborator
list visible to the governance app; an incomplete list prevents invitations.
Invitations may still be awaiting acceptance. Observation or correction
errors cause the sync to fail rather than report a clean result.
Branch protection updates are skipped when an unmanaged control could be lost
by GitHub's replacement API; those cases require manual attention.

## Release fan-out

A repository can declare downstream repositories that pin its released version
under `release_fanout` in `repos-config.yaml`. The central `release-fanout.yml`
workflow pushes a release's version straight to each target's default branch
with a GitHub App token; there are no pull requests. It runs from a
manual `workflow_dispatch`, from a product's configured or injected wrapper
when its publishing workflow completes, and from a daily backstop. The wrapper
follows workflow completion because releases created with `GITHUB_TOKEN` fire
no `release` event. Products without a wrapper still use the daily backstop. A
manual or dispatched version must be an existing published release: the
workflow rejects a tag that does not exist or is a draft or prerelease.
Upstream repositories that Actions does not manage can be declared under
`external_release_fanout`. Their releases must be selected through a manual
fan-out run. No workflow is written to the upstream repo, and the daily
backstop does not promote releases from outside Blink Labs.

```yaml
- name: blinklabs-io/dingo
  release_fanout:
    - repo: blinklabs-io/helm-charts
      type: helm-chart
      chart: charts/dingo
```

```yaml
external_release_fanout:
  - repo: txpipe/dolos
    targets:
      - repo: blinklabs-io/ansible-cardano
        type: ansible-role
        role: dolos
```

| `type` | Required field | Changes |
|---|---|---|
| `ansible-role` | `role` | `<role>_version` in `roles/<role>/defaults/main.yml` |
| `helm-chart` | `chart` | `appVersion`, `image.tag`, patch bump of the chart `version` |
| `cardano-up-package` | `package` | new `packages/<pkg>/<pkg>-<ver>.yaml` via `scripts/add-version.sh` |
| `compose-default` | `file`, `var` | the `${<var>:-X}` default in the compose `file` |
| `homebrew-formula` | `formula` | source `url`, `sha256` and `CommitHash` of the formula for tag `v<ver>` |
| `version-refs` | `refs` | version strings in the listed files |

A release tag may be `vX.Y.Z`, `vX.Y.Z.W`, or `vYYYYMMDD`, each with an
optional numeric image revision (`v11.1.3-1`). The revision is dropped, so a
rebuilt image at the same version is a no-op. Other suffixes (`-rc1`, `-pre-1`)
are rejected.

Every type also accepts `refs`, a list of globs (relative to the target repo,
`**` supported) whose version references are rewritten in the same commit.
Only the product's own image tags (`ghcr.io/blinklabs-io/<product>:X.Y.Z`), a
helm `tag:` under its `repository:`, a `<product>_version` table cell or
assignment, the last backticked version on an `image.tag` table row, backticked architecture tags (`X.Y.Z-amd64`, `-arm64`), and prose
of the form ``<Product> `X.Y.Z` `` are changed. Versions are never lowered and prereleases are ignored. The prose
rule matches any ``<Product> `X.Y.Z` ``, so `refs` globs must not include files
whose prose gives a minimum version ("0.68.0 or newer"). A target whose `refs`
are all unmatched fails the run instead of doing nothing.

For a repository declaring `release_fanout`, the engine injects
`release-fanout.yml` only when no explicit workflow or profile already declares
that file. The top-level `release_fanout_dispatch_ref` pins these injected
wrappers to a full lowercase commit SHA of `actions` containing
`reuseable-release-dispatch.yml`. An empty ref disables automatic injection.

Workflows configured directly or through a profile retain their own
`reusable_workflow` reference and are rendered even when the dispatch ref is
empty. The Docker fanout profiles use the public `shared-actions` workflow;
the dispatch ref does not change their source or pin. Every wrapper passes a
secret, so its reusable workflow reference must use a full commit SHA.

The wrapper sends a `repository_dispatch` using `ORG_PROJECT_PAT`, which needs
only Contents read/write on `blinklabs-io/actions`. The central
`.github/workflows/release-fanout.yml` receives that dispatch. Run it by hand
with the `release-fanout` workflow's `workflow_dispatch` (dry-run by default).
The daily backstop re-applies each product's latest release.

Add a new target kind by adding a handler under `scripts/fanout/handlers/`, a
case in `validateFanout` (`app/fanout.go`), a case in `apply.sh`, and tests in
`scripts/fanout/test.sh`. Run the shell tests with `bash scripts/fanout/test.sh`.

## Reconciliation trigger

Reconciliation is driven by `.github/workflows/sync.yaml`, which runs on:

- a `push` to `main` that changes `repos-config.yaml`,
- a `push` to `main` that changes the reusable Helm publisher,
- a manual `workflow_dispatch`, or
- a daily `schedule`. The scheduled run corrects drift and, when
  [auto-discovery](#auto-discovery-opt-in) is enabled, picks up newly-created
  marker repositories without a `repos-config.yaml` change.

The sync job runs only on `main` for push, schedule, and manual events. It
verifies that the checked-out revision is still current before minting a GitHub
App token. Reruns are rejected; start a new run after a failure. Lifecycle writes
check the current `main` revision again. These checks do not make an entire
reconciliation batch atomic if `main` changes while it is running.

This private controller uses GitHub Free and does not require Core environment
approval. Its committed configuration explicitly enables:

```yaml
github_free: true
```

This mode skips the controller's lifecycle-state ruleset requirement and skips
branch-protection reconciliation for targets observed as private through the
API. Private protection skips appear only in aggregate skipped counts; they are
not successful protection updates. Public target protections remain enforced,
and their API failures remain errors. Settings and team access still reconcile
for private targets. Omitting `github_free` or setting it to `false` restores
state-ruleset checks and private target protection reconciliation; it does not
add an environment approval gate to the workflows.

Automatic archival remains enabled for applied `archive_on_removal` markers.
State signatures, repository identity checks, configuration-history checks, and
compare-and-swap state updates remain in place. **Repository writers must be
trusted:** a signature detects modified state but cannot prevent replay of a
previously valid state record. GitHub Free cannot enforce the state ruleset in
a private controller. Writers who can change workflows also have access to the
repository-level App credentials through those workflows. The run-context check
is not an independent approval or a boundary against malicious workflow changes.

GitHub documents the availability of [rulesets](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/about-rulesets)
and [environment protection](https://docs.github.com/en/actions/how-tos/deploy/configure-and-manage-deployments/manage-environments).
No paid plan is required by this configuration.

The GitHub App installation must cover all repositories in the controller
organization: new repositories do not exist when the token is minted, and an
archival target is no longer in the current configuration. The token is limited
to administration, contents, and workflow write permissions plus organization
Members read access for configured team grants. Pull request
checks do not run the sync.

The Helm Charts and `repository-standard-pinned` profiles use reviewed,
immutable workflow revisions from the public `blinklabs-io/shared-actions`
repository. Update their sources in `repos-config.yaml`; governance generates
the caller wrappers. Where default-branch rules require pull requests, merge
downstream caller changes through reviewed pull requests before the
corresponding configuration change. Include this repository's generated
Conventional Commits wrapper in the same pull request as its configuration.

The Helm publisher rejects changed charts whose versions did not increase and
skips a retry when published chart contents already match.

## Configuration

The sync engine requires these environment variables:

| Variable | Description |
|---|---|
| `GH_TOKEN` | GitHub App installation token with administration, contents, and workflows write permissions, organization Members read permission, and access to all organization repositories. |
| `GITHUB_REPOSITORY` | The controller repository, supplied by GitHub Actions. |
| `GITHUB_SHA` | The checked-out current `main` revision, supplied by GitHub Actions. |
| `GOVERNANCE_LIFECYCLE_STATE_KEY` | Stable secret material used to authenticate lifecycle state and conceal repository identities. The workflow supplies the existing `GOVERNANCE_APP_PRIVATE_KEY` secret. |

No separate lifecycle secret is required. The workflow passes the existing
governance App private-key secret to the engine's `GOVERNANCE_LIFECYCLE_STATE_KEY`
environment variable. The engine uses those secret bytes for HMAC-SHA256 state
authentication; it does not publish the private key or use the short-lived
installation token as state-key material.

State authentication depends on the exact secret bytes. Rotating the App key,
reformatting its PEM, or switching an existing state record from another key
requires an explicit state migration before sync can continue. A signature
mismatch fails closed; the engine never silently resets existing state.

Governance sync, release fan-out, and the
[infrastructure release pin updater](docs/release-pin-updater.md) use the existing repository secrets
`GOVERNANCE_APP_CLIENT_ID` and `GOVERNANCE_APP_PRIVATE_KEY` for the governance
GitHub App.

When `github_free` is false or omitted, protect
`refs/heads/governance-applied-state` with an active repository ruleset
named `governance-lifecycle-state`. Restrict creation, updates, deletion, and
non-fast-forward updates, with only the governance GitHub App as an always-on
bypass actor. The engine checks this ruleset before lifecycle work unless `github_free`
explicitly selects trusted-writer operation.
The branch stores only authenticated revision and identity hashes, never
repository names. Protect and retain the state key: losing or rotating it makes
the existing state unreadable and requires deliberately re-arming archival
markers. The first successful run establishes state without archiving anything.
If a marked repository is renamed, deleted, or replaced before removal, the
run fails without archiving it and requires operator intervention. Do not
re-add a deleted repository unless recreating it is intended.

Release fan-out remains automatic on `main` without an environment dependency.
It checks the current main revision before requesting its target-scoped token.
The App needs access to each target repository and any branch-protection bypass
required to push to its default branch.

Both workflows use the existing repository-level App key. Fan-out requests a
contents-write token scoped to its target repository, but the key can request
the App's broader installation permissions. See GitHub's
[installation token documentation](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-an-installation-access-token-for-a-github-app).

## Managing repositories

Most repositories are described using a **profile** — a reusable bundle of
settings, collaborators, branch protection, and workflows defined once under
`profiles:` and shared across many repos. A repository then only supplies the
profile name plus its own `vars` (and, rarely, `overrides`):

```yaml
profiles:
  docker-standard:
    settings:
      delete_branch_on_merge: true
    branch_protection:
      - branch: main
        required_status_checks: []
    workflows:
      - destination_file: ci-docker.yml
        workflow_name: "Docker CI"
        reusable_workflow: "blinklabs-io/actions/.github/workflows/reuseable-ci-docker-multiarch.yml@main"
        triggers:
          pull_request:
            branches: [main]
        params:
          image-name: "blinklabs-io/${image}"

repositories:
  # Profile-based: inherits everything from docker-standard, supplies only vars.
  - name: blinklabs-io/example
    profile: docker-standard
    vars:
      image: example
      description: "Example image"
    # Optional: patch individual profile workflows (keyed by destination_file).
    # overrides:
    #   ci-docker.yml:
    #     params:
    #       image-name: "blinklabs-io/custom"
```

Profile-based repositories inherit `settings`, `collaborators`,
`branch_protection`, `workflows`, and `dependabot` from the profile and must
**not** set those fields directly — doing so is rejected as a configuration
error. Use `vars` for substitution and `overrides` for per-workflow tweaks.

A repository or profile can also set `dependabot` to a YAML block containing
the complete Dependabot manifest. The engine validates its required fields and
reconciles it to `.github/dependabot.yml`:

```yaml
repositories:
  - name: blinklabs-io/example
    dependabot: |
      version: 2
      updates:
        - package-ecosystem: gomod
          directory: /
          schedule:
            interval: weekly
```

An omitted or empty `dependabot` value leaves an existing
`.github/dependabot.yml` untouched. This lets repositories keep manifests they
manage outside the central configuration; removing a central value does not
delete a repository file.

The Docker profiles manage weekly update schedules for their repositories.
`docker-standard` covers Docker and GitHub Actions; `docker-actions-only` and
`docker-deps-prefix` preserve the different configurations of their existing
users. Reconciliation replaces managed manifests, so review each repository's
existing settings before assigning it to one of these profiles.

For a genuinely one-off repository, omit `profile` and specify the full schema
explicitly instead:

```yaml
repositories:
  - name: blinklabs-io/special
    settings:
      delete_branch_on_merge: true
    collaborators:
      - username: alice
        permission: write
    branch_protection:
      - branch: main
        required_status_checks:
          - build
    workflows:
      - destination_file: ci-docker.yml
        workflow_name: "Docker CI"
        reusable_workflow: "blinklabs-io/actions/.github/workflows/reuseable-ci-docker-multiarch.yml@main"
        triggers:
          pull_request:
            branches: [main]
        params:
          image-name: "blinklabs-io/special"
```

To onboard or change a repository, edit `repos-config.yaml` and push to `main`.
An explicitly listed repository that does not exist is created with an initial
README. New repositories are public by default. The `private: true` setting
controls creation only and does not change an existing repository's visibility.
The organization's default branch must match any branch-protection names in its
configuration.

This controller is private. Declare private repositories directly in
`repos-config.yaml`; a separate configuration source is not required:

```yaml
repositories:
  - name: blinklabs-io/example-private
    private: true
    profile: repository-standard
```

The example name is a placeholder. `private` belongs on the repository entry,
alongside `profile`, not inside `settings`. The `repository-standard` profile
configures automatic branch deletion after merge, protection for `main`, and the
Conventional Commits workflow. Other profiles can supply collaborators,
workflows, and Dependabot configuration. Profile-based entries inherit these
fields; do not duplicate them on the repository entry.

The generated workflows also need permission to call this private repository's
reusable workflows. Configure its GitHub Actions access policy for the intended
private callers. Public repositories cannot call reusable workflows hosted in a
private repository; see [GitHub's reusable workflow access rules](https://docs.github.com/en/actions/reference/workflows-and-actions/reusing-workflow-configurations).

GitHub requires Team or Enterprise for branch protection in private organization
repositories. A private repository can be created on Free, but applying a profile
with branch protection fails unless `github_free: true` explicitly skips
private protection. Such skips are counted in the drift report and do not mean
that the branch is protected. See [GitHub's protected branch availability](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-protected-branches).

Verify this controller remains private before committing confidential entries.
Keep private repository names and configuration out of public repositories and
their generated files, workflow inputs, logs, and artifacts. Fan-out can include
repository names in job metadata, so check the visibility of each execution
destination. Credentials remain in GitHub secrets. Drift reports continue to
show counts only for private or unknown-visibility repositories.

To archive an explicitly managed repository, first add
`archive_on_removal: true` to its entry and allow that configuration to be
successfully applied. Remove the entry in a later configuration change and
start another sync; the engine verifies the same repository identity and
archives it. Removing the marker, or removing and re-adding the entry, cancels
the old archival intent. Removing an unmarked entry only stops management.
Forks, the controller repository, and repositories still managed by discovery
are never archived by this transition. A failed operation fails the run and is
safe to retry with a new workflow run.

If an intermediate configuration revision has invalid YAML or an invalid
explicit repository list, the next successful sync cancels prior archival
eligibility and records markers in the current configuration as new intent.
Archival then requires a later removal and successful sync. Invalid current or
last-applied configurations and Git history read failures still stop the run.

Each run reconciles all configured and discovered repositories except those
with `sync_paused: true`. A pause cancels prior archival intent. To re-arm
archival, resume sync with `archive_on_removal: true` and let that run succeed
before removing the entry.

The `actions` repository is included in the managed set. `repository-standard`
applies the shared repository settings, `main` branch protection, and
Conventional Commits check to repositories whose CI and release workflows have
not yet been migrated to shared workflows. The explicit public-repository set
covers active, non-fork repositories; archived repositories are read-only, and
forks are excluded from governance writes.

### Concurrency

A workflow, or the one member of a pipeline that declares it, can set a
`concurrency` block so a new push cancels the run for the commit it replaced:

```yaml
concurrency:
  group: "${{ github.workflow }}-${{ github.event.pull_request.number || github.ref }}"
  cancel_in_progress: "true"
```

`cancel_in_progress` is `true`, `false`, or a single `${{ }}` expression. A
pipeline has one `concurrency:` block, so declaring it on more than one member
is rejected. A per-repository `overrides` entry replaces the profile's block.

## Auto-discovery (opt-in)

Instead of listing every repository in `repos-config.yaml`, the engine can
discover managed repositories directly from the controller organization.
Discovery is **disabled by default**; enable it with a `discovery` block. The
`repositories` list is still required and may be empty:

```yaml
discovery:
  enabled: true
  organization: blinklabs-io
  # Optional: only consider repositories carrying this GitHub topic.
  topic: blinklabs-managed
  # Optional: path to the in-repo marker file (default: .blinklabs/profile.yml).
  marker_path: .blinklabs/profile.yml
```

When enabled, the engine lists the organization's repositories, skips archived
ones, optionally filters by `topic`, and reads a **marker file** from each
repository's default branch. A repository opts in by committing a marker at
`.blinklabs/profile.yml`:

```yaml
# .blinklabs/profile.yml — lives in the managed repository, not here.
profile: docker-standard
vars:
  # Basename only — the profile prepends the registry namespace (e.g.
  # `blinklabs-io/` and `blinklabs/`), so do not include it here.
  image: example
  description: Example service image
# Optional per-workflow overrides, same schema as repos-config.yaml.
overrides:
  publish.yml:
    params:
      build-target: example
```

Discovered repositories are merged with the explicit `repositories:` list.
**Explicit entries always win** — if a repository appears in both places, its
`repos-config.yaml` definition is used and the marker is ignored. Repositories
without a valid marker (missing file or missing `profile`) are skipped.

## Local development

```bash
# Format, vet, and test
gofmt -w app/
go vet ./...
go test ./... -count=1

# Run the engine locally (writes to real repositories — use a scoped token)
GH_TOKEN=<token> go run ./app
```

> Running `go run ./app` reconciles live repositories. Use a token scoped to a
> test repository when experimenting.

### Repository settings and team grants

Repository entries and profiles can declare `teams` using organization team
slugs and `pull` (read), `triage`, `push` (write), `maintain`, or `admin`
permissions. `read` and `write` aliases are also accepted. Governance updates
only listed grants, leaves unlisted teams alone, and never changes team
membership. An empty or omitted list leaves all team access unmanaged.
Profile-based repositories inherit team grants; an explicit nonempty team list
cannot be combined with a profile.

```yaml
teams:
  - slug: core
    permission: push
settings:
  delete_branch_on_merge: true
  allow_merge_commit: false
  allow_squash_merge: true
  allow_rebase_merge: false
```

Supported optional `settings` fields are:

| Group | Fields |
|---|---|
| Metadata | `description`, `homepage` |
| Default branch | `default_branch` (selects an existing branch; does not create or rename it) |
| Features | `has_issues`, `has_projects`, `has_wiki` |
| Pull requests | `allow_auto_merge`, `allow_update_branch`, `delete_branch_on_merge` |
| Merge methods | `allow_merge_commit`, `allow_squash_merge`, `allow_rebase_merge` |
| Web commits | `web_commit_signoff_required` |
| Commit defaults | `squash_merge_commit_title`, `squash_merge_commit_message`, `merge_commit_title`, `merge_commit_message` |

Omitted settings are left unchanged. Explicit `false` disables a configured
boolean; an empty `description` or `homepage` clears that metadata. Unsupported
settings are rejected. Repository creation visibility remains the top-level
`private` field; rename, archival, Actions policies, and security policies are
not controlled by `settings`.

Commit-message values follow GitHub's API enums. Configure the corresponding
`*_commit_title` whenever setting `*_commit_message`; the title is included in
the update even if it already matches. All three merge methods cannot be
explicitly disabled together. GitHub validates branch existence and any
organization or plan restrictions, and rejected updates fail the sync.
When linear-history protection is configured, explicitly enabled squash or
rebase merge methods are applied first to satisfy GitHub's
[linear-history prerequisite](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-protected-branches/about-protected-branches#require-linear-history).
Those methods remain enabled if protection fails. Configured branch protections
are then applied before default-branch selection. Failed merge-method prerequisites
prevent protection updates; failed protection or settings updates prevent managed
workflow publication. New team grants require successful repository controls.
YAML aliases and merge keys remain supported, including anchors outside the
settings block.

Configured values are compared against an API observation before patching only
changed fields and their required companion fields. Missing or incorrectly
typed fields fail the observation; a documented null description or homepage
means empty metadata.

Team grants are read before updates and verified afterward; failed or incomplete
observations fail the sync rather than being treated as missing grants. Drift reports preserve the
existing private-repository redaction.

The governance App and its organization installation must grant **Members:
read**, in addition to repository **Administration: write** and **Metadata:
read**, before running the sync with team grants. The sync token explicitly
requests Members read; the App's existing private key remains usable after the
permission change is accepted. See GitHub's [team repository permissions
API](https://docs.github.com/en/rest/teams/teams#add-or-update-team-repository-permissions)
and [repository settings API](https://docs.github.com/en/rest/repos/repos#update-a-repository).

`shared-actions` is provisioned as a public repository with team grants and
main-branch protection. Its initial configuration does not generate callers of
private reusable workflows. Publishing the shared CI implementation and
migrating callers are separate changes.
