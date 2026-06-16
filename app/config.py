"""Default StandardizerConfig with all known Blink Labs CI workflow definitions."""

from __future__ import annotations

import os

from .types import StandardizerConfig, WorkflowDefinition

_ISSUE_CLOSED_TRIGGER = """\
on:
  issues:
    types:
      - closed"""


def get_config() -> StandardizerConfig:
    return StandardizerConfig(
        org=os.environ.get("GITHUB_ORG", "blinklabs-io"),
        central_repo=os.environ.get("GITHUB_CENTRAL_REPO", "actions"),
        central_ref=os.environ.get("GITHUB_CENTRAL_REF", "main"),
        branch_prefix="blinklabs/standardize-ci",
        commit_message="Standardize CI workflow definitions",
        workflows=[
            WorkflowDefinition(
                id="test-issue-on-close",
                name="Test Issue Close Trigger",
                target_path=".github/workflows/test-issue-on-close.yml",
                match_file_names=[
                    "test-issue-on-close.yml",
                    "test-issue-on-close.yaml",
                ],
                match_name_includes=["test issue close", "issue close trigger"],
                central_workflow_path=".github/workflows/reuseable-test-issue-on-close.yml",
                job_name="test",
                trigger=_ISSUE_CLOSED_TRIGGER,
                permissions={"contents": "read"},
                with_inputs={
                    "issue_number": "${{ github.event.issue.number }}",
                    "issue_title": "${{ github.event.issue.title }}",
                },
                secrets="",
            ),
            WorkflowDefinition(
                id="set-project-closed-date",
                name="Set Project Closed Date",
                target_path=".github/workflows/set-project-closed-date.yml",
                match_file_names=[
                    "set-project-closed-date.yml",
                    "set-project-closed-date.yaml",
                ],
                match_name_includes=["set project closed date", "project closed date"],
                central_workflow_path=".github/workflows/reuseable-set-project-closed-date.yml",
                job_name="set-date",
                trigger=_ISSUE_CLOSED_TRIGGER,
                permissions={"contents": "read", "issues": "read"},
                with_inputs={
                    "project_url": "${{ vars.PROJECT_URL }}",
                    "closed_date_field": "Closed Date",
                    "closed_at": "${{ github.event.issue.closed_at }}",
                },
                secrets={"project_pat": "${{ secrets.ORG_PROJECT_PAT }}"},
            ),
        ],
    )


DEFAULT_CONFIG: StandardizerConfig = get_config()
