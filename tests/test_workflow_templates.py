"""Tests for app.workflow_templates using PyYAML for parsing assertions."""

from __future__ import annotations

import pytest
import yaml

from app.config import DEFAULT_CONFIG
from app.types import WorkflowFile
from app.workflow_templates import (
    find_workflow_for_definition,
    generate_standard_workflow,
    is_standardized_workflow,
    read_workflow_name,
)


class TestGenerateStandardWorkflow:
    def test_test_issue_on_close_uses_reference(self):
        content = generate_standard_workflow(DEFAULT_CONFIG.workflows[0], DEFAULT_CONFIG)
        assert (
            "uses: blinklabs-io/actions/.github/workflows/reuseable-test-issue-on-close.yml@main"
            in content
        )

    def test_test_issue_on_close_no_explicit_secrets(self):
        content = generate_standard_workflow(DEFAULT_CONFIG.workflows[0], DEFAULT_CONFIG)
        assert "secrets" not in content

    def test_expression_inputs_passed_verbatim(self):
        content = generate_standard_workflow(DEFAULT_CONFIG.workflows[0], DEFAULT_CONFIG)
        assert "issue_number: ${{ github.event.issue.number }}" in content
        assert "issue_title: ${{ github.event.issue.title }}" in content

    def test_permissions_block_contents_read(self):
        content = generate_standard_workflow(DEFAULT_CONFIG.workflows[0], DEFAULT_CONFIG)
        assert "permissions:" in content
        assert "contents: read" in content

    def test_set_project_closed_date_uses_reference(self):
        content = generate_standard_workflow(DEFAULT_CONFIG.workflows[1], DEFAULT_CONFIG)
        assert (
            "uses: blinklabs-io/actions/.github/workflows/reuseable-set-project-closed-date.yml@main"
            in content
        )

    def test_set_project_closed_date_explicit_secrets(self):
        content = generate_standard_workflow(DEFAULT_CONFIG.workflows[1], DEFAULT_CONFIG)
        assert "project_pat:" in content
        assert "secrets: inherit" not in content

    def test_set_project_closed_date_with_inputs(self):
        content = generate_standard_workflow(DEFAULT_CONFIG.workflows[1], DEFAULT_CONFIG)
        assert "closed_at: ${{ github.event.issue.closed_at }}" in content
        assert "project_url: ${{ vars.PROJECT_URL }}" in content

    def test_workflow_name_in_output(self):
        content = generate_standard_workflow(DEFAULT_CONFIG.workflows[0], DEFAULT_CONFIG)
        assert content.startswith("name: Test Issue Close Trigger")

    def test_output_ends_with_newline(self):
        content = generate_standard_workflow(DEFAULT_CONFIG.workflows[0], DEFAULT_CONFIG)
        assert content.endswith("\n")


class TestIsStandardizedWorkflow:
    def test_returns_true_for_generated_content(self):
        for defn in DEFAULT_CONFIG.workflows:
            content = generate_standard_workflow(defn, DEFAULT_CONFIG)
            assert is_standardized_workflow(content, defn, DEFAULT_CONFIG)

    def test_returns_false_for_inline_workflow(self):
        inline = (
            "name: Test Issue Close Trigger\n"
            "on:\n  issues:\n    types: [closed]\n"
            "jobs:\n  test:\n    runs-on: ubuntu-latest\n"
        )
        assert not is_standardized_workflow(
            inline, DEFAULT_CONFIG.workflows[0], DEFAULT_CONFIG
        )

    def test_returns_false_for_wrong_ref(self):
        content = generate_standard_workflow(DEFAULT_CONFIG.workflows[0], DEFAULT_CONFIG)
        content = content.replace("@main", "@v1")
        assert not is_standardized_workflow(
            content, DEFAULT_CONFIG.workflows[0], DEFAULT_CONFIG
        )


class TestFindWorkflowForDefinition:
    def _file(self, path: str, content: str = "") -> WorkflowFile:
        return WorkflowFile(path=path, sha="abc", content=content)

    def test_exact_path_match_test_issue(self):
        defn = DEFAULT_CONFIG.workflows[0]  # test-issue-on-close
        files = [self._file(defn.target_path)]
        result = find_workflow_for_definition(files, defn)
        assert result is not None
        assert result.path == defn.target_path

    def test_filename_alias_match_test_issue(self):
        defn = DEFAULT_CONFIG.workflows[0]  # test-issue-on-close
        files = [self._file(".github/workflows/test-issue-on-close.yml")]
        result = find_workflow_for_definition(files, defn)
        assert result is not None

    def test_display_name_match_set_project_closed_date(self):
        defn = DEFAULT_CONFIG.workflows[1]  # set-project-closed-date
        files = [
            self._file(
                ".github/workflows/close.yml",
                "name: Set Project Closed Date\non: issues\n",
            )
        ]
        result = find_workflow_for_definition(files, defn)
        assert result is not None
        assert result.path == ".github/workflows/close.yml"

    def test_no_match_returns_none(self):
        defn = DEFAULT_CONFIG.workflows[0]
        files = [self._file(".github/workflows/release.yml", "name: Release\n")]
        assert find_workflow_for_definition(files, defn) is None

    def test_exact_path_takes_priority_over_name(self):
        defn = DEFAULT_CONFIG.workflows[0]
        files = [
            self._file(
                ".github/workflows/other.yml", "name: Test Issue Close Trigger\n"
            ),
            self._file(defn.target_path, "name: Something Else\n"),
        ]
        result = find_workflow_for_definition(files, defn)
        assert result is not None
        assert result.path == defn.target_path


class TestReadWorkflowName:
    def test_returns_lowercase_name(self):
        assert read_workflow_name("name: CI Dockerfile\non: push\n") == "ci dockerfile"

    def test_returns_empty_string_on_invalid_yaml(self):
        assert read_workflow_name("name: [") == ""

    def test_returns_empty_string_on_empty_content(self):
        assert read_workflow_name("") == ""

    def test_returns_empty_string_when_name_not_string(self):
        assert read_workflow_name("name:\n  - item\n") == ""
