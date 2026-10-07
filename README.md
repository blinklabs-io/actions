# Blink Labs Actions

This repository contains the reusable GitHub Actions workflows shared across
Blink Labs repositories, plus a small **Go governance engine** that keeps every
repository aligned to a single declarative source of truth: `repos-config.yaml`.

The engine reads `repos-config.yaml`, and for each listed repository it
reconciles repository settings, collaborators, branch protection, and the
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
- `reuseable-publish-desktop.yml` — full release pipeline for desktop-style Go apps that ship a Fyne GUI tray alongside a CLI: macOS signed + notarized `.pkg` installers, Windows signed `.msi` installers (jsign + WiX), Linux/FreeBSD tarballs, native multi-arch Docker images, and build-provenance attestations for every binary, installer, and image. The packaging scripts and `make` targets it drives live in the calling repository.
- `reuseable-go-test.yml` — Go module verification, tidy checks, build, vet, tests, in-job golangci-lint and NilAway, Buf checks, generated-code drift checks, forbidden-import guards, nested modules, service containers, additional test passes, and explicit cross-build targets; supports an optional Linux race-detection pass. `test-tags` applies to the build, vet, cross-build and every test pass (including nested modules), `test-ldflags` to every test pass, and `build-flags` to the build and cross-build (`test-ldflags` exists because `test-flags` is split on whitespace and cannot carry a quoted `-ldflags` value), and `timeout-minutes` sets the job backstop.
- `reuseable-golangci-lint.yml` — golangci-lint with a configurable tool version, additional invocation, optional formatting-diff check, nested modules (`additional-working-directories`), and a `pre-lint-command` for repository checks that belong in the lint stage.
- `reuseable-nilaway.yml` — NilAway static analysis.
- `reuseable-govulncheck.yml` — govulncheck, with an optional gosec SARIF upload; `govulncheck-command` substitutes a repository's own scan (for example a Makefile target) for the default `go run` invocation.
- `reuseable-conventional-commits.yml` — conventional-commit PR title check.
- `reuseable-check-versions.yml` — check upstream image versions against a docker-compose file and open update PRs.
- `reuseable-check-versions-packages.yml` — check upstream release/image versions against `packages/<pkg>/<pkg>-*.yaml` version files, validate with the cardano-up CLI, and open update PRs (for cardano-up package repositories).
- `reuseable-package-validate.yml` — validate cardano-up package definitions with the cardano-up CLI (plus optional filename/content version-consistency check).

The reusable Go test workflow starts cross-builds only after tests pass and limits them to two concurrent targets by default. Callers can set `cross-build-max-parallel` to choose another limit.

## How the engine works

The engine (`app/main.go`) runs as a batch job — there is no long-running
server or webhook listener. For each repository in `repos-config.yaml` it
performs an observe → compare → act loop:

1. `syncRepoSettings` — reconciles repository settings (e.g. `delete_branch_on_merge`).
2. `syncCollaborators` — reconciles collaborators and their permission levels.
3. `syncBranchProtection` — reconciles branch protection rules.
4. `syncWorkflows` — renders standalone wrappers from `templates/workflow.tmpl`
   and grouped pipelines from `templates/pipeline.tmpl`. Configured workflows
   can replace old wrappers in the same commit. Files that already match are
   skipped.

Writes go **directly to each repository's default branch**; the engine does not
open pull requests.

## Reconciliation trigger

Reconciliation is driven by `.github/workflows/sync.yaml`, which runs on:

- a `push` to `main` that changes `repos-config.yaml`,
- a manual `workflow_dispatch`, or
- a daily `schedule`. The scheduled run corrects drift and, when
  [auto-discovery](#auto-discovery-opt-in) is enabled, picks up newly-created
  marker repositories without a `repos-config.yaml` change.

The sync workflow:

1. Parses `repos-config.yaml` to resolve the list of target repositories.
2. Mints a GitHub App installation token via `actions/create-github-app-token`.
   By default the token is scoped **only** to the repositories listed in
   `repos-config.yaml`, limiting blast radius. When discovery is enabled the
   token is scoped org-wide (all repositories the installation can access),
   since the target set is not known ahead of time.
3. Runs the engine with `go run ./app`, passing the token as `GH_TOKEN`.

## Configuration

The engine requires a single environment variable:

| Variable | Description |
|---|---|
| `GH_TOKEN` | **Required.** A token with `contents: write`, `workflows: write`, and `administration`/`metadata` access to the managed repositories. `workflows: write` is required because the engine writes files under `.github/workflows`, which GitHub gates behind the separate Workflows permission. In CI this is the scoped GitHub App token minted by `sync.yaml`. |

The sync workflow additionally reads these repository secrets:

| Secret | Description |
|---|---|
| `GOVERNANCE_APP_CLIENT_ID` | Client ID of the governance GitHub App. |
| `GOVERNANCE_APP_PRIVATE_KEY` | PEM private key for the governance GitHub App. |

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
Each reconciliation run processes **every** managed repository (all configured
and discovered targets), not only the ones touched by the edit. The
`actions` repository is included in the managed set. `repository-standard`
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
discover managed repositories directly from the organization. Discovery is
**disabled by default**; enable it with a `discovery` block:

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
