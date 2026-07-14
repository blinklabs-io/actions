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

```
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

- `reuseable-ci-docker-multiarch.yml` — native multi-arch Docker CI build (no QEMU).
- `reuseable-publish-docker-multiarch.yml` — native multi-arch Docker publish to Docker Hub + GHCR, with build provenance attestations always enabled.
- `reuseable-publish.yml` — Go binary + native multi-arch Docker image publish for Go services.
- `reuseable-go-test.yml` — Go test suite.
- `reuseable-golangci-lint.yml` — golangci-lint.
- `reuseable-nilaway.yml` — NilAway static analysis.
- `reuseable-conventional-commits.yml` — conventional-commit PR title check.
- `reuseable-set-project-closed-date.yml` — writes the issue closed date to a GitHub Project field.

## How the engine works

The engine (`app/main.go`) runs as a batch job — there is no long-running
server or webhook listener. For each repository in `repos-config.yaml` it
performs an observe → compare → act loop:

1. `syncRepoSettings` — reconciles repository settings (e.g. `delete_branch_on_merge`).
2. `syncCollaborators` — reconciles collaborators and their permission levels.
3. `syncBranchProtection` — reconciles branch protection rules.
4. `syncWorkflows` — renders each workflow wrapper from `templates/workflow.tmpl`
   and, if the rendered content differs from what is already on the repository's
   default branch, writes it via the GitHub Contents API. Files that already
   match are skipped.

Writes go **directly to each repository's default branch**; the engine does not
open pull requests.

## Reconciliation trigger

Reconciliation is driven by `.github/workflows/sync.yaml`, which runs on:

- a `push` to `main` that changes `repos-config.yaml`, or
- a manual `workflow_dispatch`.

The sync workflow:

1. Parses `repos-config.yaml` to resolve the list of target repositories.
2. Mints a GitHub App installation token scoped **only** to those repositories
   (via `actions/create-github-app-token`), limiting blast radius.
3. Runs the engine with `go run ./app`, passing the token as `GH_TOKEN`.

## Configuration

The engine requires a single environment variable:

| Variable | Description |
|---|---|
| `GH_TOKEN` | **Required.** A token with `contents: write` and `administration`/`metadata` access to the managed repositories. In CI this is the scoped GitHub App token minted by `sync.yaml`. |

The sync workflow additionally reads these repository secrets:

| Secret | Description |
|---|---|
| `GOVERNANCE_APP_CLIENT_ID` | Client ID of the governance GitHub App. |
| `GOVERNANCE_APP_PRIVATE_KEY` | PEM private key for the governance GitHub App. |

## Managing repositories

Each repository is described declaratively in `repos-config.yaml`:

```yaml
repositories:
  - name: blinklabs-io/example
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
          image-name: "blinklabs-io/example"
```

To onboard or change a repository, edit `repos-config.yaml` and push to `main`.
The sync workflow reconciles the affected repositories automatically. The
`actions` repository manages other repositories and is not part of the managed
set.

## Local development

```bash
# Format, vet, and test
gofmt -l app/
go vet ./...
go test ./... -count=1

# Run the engine locally (writes to real repositories — use a scoped token)
GH_TOKEN=<token> go run ./app
```

> Running `go run ./app` reconciles live repositories. Use a token scoped to a
> test repository when experimenting.
