# Blink Labs Actions

This repository contains reusable GitHub Actions workflows and a Python GitHub App that keeps all Blink Labs repositories aligned to those centralized workflows.

## Runtime requirements

- Python ≥ 3.9
- `github3.py==4.0.1` — GitHub REST API client
- `GitPython==3.1.48` — reads central workflow templates from the local git repository
- `PyYAML==6.0.3` — parses existing workflow files for drift detection

## Reusable workflows

Central workflow templates live in `.github/workflows` and are called from downstream repositories via `workflow_call`.

- `.github/workflows/reuseable-test-issue-on-close.yml` — shared issue-close test trigger.
- `.github/workflows/reuseable-set-project-closed-date.yml` — writes the issue closed date to a GitHub Project field.

Example downstream wrapper (generated automatically by the app):

```yaml
name: Test Issue Close Trigger

on:
  issues:
    types:
      - closed

jobs:
  test:
    uses: blinklabs-io/actions/.github/workflows/reuseable-test-issue-on-close.yml@main
    permissions:
      contents: read
    with:
      issue_number: ${{ github.event.issue.number }}
      issue_title: ${{ github.event.issue.title }}
```

## GitHub App — how it works

The app reconciles drift in three ways:

- **`push` webhook** — reconciles the repository each time code is pushed to the default branch.
- **`installation_repositories.added` webhook** — reconciles newly installed repositories immediately.
- **CLI / manual run** — `python main.py reconcile` for on-demand or scheduled reconciliation.

For each repository with drift the app:
1. Creates (or reuses) a branch `blinklabs/standardize-ci-YYYY-MM-DD`.
2. Writes standardized workflow wrapper files under `.github/workflows`.
3. Reports the branch name so a repository owner can open and review a pull request manually.

The `actions` repository itself is always skipped.

## Required GitHub App permissions

Repository permissions:
- `Contents`: read and write
- `Metadata`: read

Webhook events to subscribe:
- `push`
- `installation_repositories`

## Configuration

```bash
cp .env.example .env
# Fill in GITHUB_APP_ID, GITHUB_PRIVATE_KEY, GITHUB_WEBHOOK_SECRET
```

| Variable | Default | Description |
|---|---|---|
| `GITHUB_APP_ID` | — | **Required.** GitHub App ID. |
| `GITHUB_PRIVATE_KEY` | — | **Required.** PEM private key (`\n`-escaped). |
| `GITHUB_WEBHOOK_SECRET` | — | **Required.** Webhook secret for HMAC-SHA256 verification. |
| `GITHUB_ORG` | `blinklabs-io` | GitHub organisation name. |
| `GITHUB_CENTRAL_REPO` | `actions` | Name of this central repository. |
| `GITHUB_CENTRAL_REF` | `main` | Git ref used in `uses:` references. |
| `APP_PORT` | `3000` | Port the webhook server listens on. |
| `DRY_RUN` | `false` | When `true`, compute diff but make no writes. |

## Local development

```bash
# Create virtual environment and install dependencies
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt -r requirements-dev.txt

# Run tests
python -m pytest tests/ -v

# Start the webhook server
python main.py serve

# Dry-run reconciliation against one installation
python main.py reconcile --installation-id <id> --dry-run

# Limit to a single repository
python main.py reconcile --installation-id <id> --repo dingo
```

## Project layout

```
app/
  types.py             # Dataclasses (WorkflowDefinition, ReconcileResult, …)
  config.py            # Default StandardizerConfig with all known workflows
  workflow_templates.py# YAML generation (string templates) + PyYAML parsing
  reconciler.py        # Pure reconciliation logic, decoupled from github3.py
  github_client.py     # github3.py wrapper + GitPython local-repo helpers
  server.py            # stdlib HTTP webhook server (HMAC-SHA256 verified)
  cli.py               # argparse CLI for manual reconciliation
main.py                # Entry point: `serve` or `reconcile`
tests/
  test_workflow_templates.py
  test_reconciler.py
.github/workflows/     # Central reusable workflow templates
```
