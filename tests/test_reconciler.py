"""Tests for app.reconciler using an in-memory fake client."""

from __future__ import annotations

from typing import Optional

from app.config import DEFAULT_CONFIG
from app.reconciler import reconcile_repository
from app.types import RepositoryRef, WorkflowFile
from app.workflow_templates import generate_standard_workflow


# ---------------------------------------------------------------------------
# Fake client
# ---------------------------------------------------------------------------


class FakeRepositoryClient:
    """In-memory GitHubRepositoryClient implementation for tests."""

    def __init__(self, files: list[WorkflowFile] | None = None) -> None:
        self._files: list[WorkflowFile] = files or []
        self.writes: list[dict] = []
        self._branches: list[str] = []

    def list_workflow_files(self, repository: RepositoryRef) -> list[WorkflowFile]:
        return list(self._files)

    def get_default_branch_sha(self, repository: RepositoryRef) -> str:
        return "base-sha-abc123"

    def ensure_branch(self, repository: RepositoryRef, branch: str, sha: str) -> None:
        self._branches.append(branch)

    def upsert_file(
        self,
        repository: RepositoryRef,
        branch: str,
        path: str,
        content: str,
        message: str,
        sha: Optional[str] = None,
    ) -> None:
        self.writes.append({"path": path, "content": content, "sha": sha})


# ---------------------------------------------------------------------------
# Test fixtures
# ---------------------------------------------------------------------------

_REPO = RepositoryRef(owner="blinklabs-io", repo="dingo", default_branch="main")


def _already_standardized_files() -> list[WorkflowFile]:
    return [
        WorkflowFile(
            path=defn.target_path,
            sha=defn.id,
            content=generate_standard_workflow(defn, DEFAULT_CONFIG),
        )
        for defn in DEFAULT_CONFIG.workflows
    ]


# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------


class TestReconcileRepository:
    def test_no_changes_when_already_standardized(self):
        client = FakeRepositoryClient(_already_standardized_files())
        result = reconcile_repository(client, DEFAULT_CONFIG, _REPO)
        assert result.changed_files == []
        assert client.writes == []
        assert result.branch_name is None

    def test_detects_drift_for_all_missing_workflows(self):
        client = FakeRepositoryClient([])
        result = reconcile_repository(client, DEFAULT_CONFIG, _REPO)
        assert set(result.changed_files) == {
            defn.target_path for defn in DEFAULT_CONFIG.workflows
        }

    def test_returns_branch_name_on_drift(self):
        """reconciler must return the branch it wrote to — no PR is created."""
        client = FakeRepositoryClient([])
        result = reconcile_repository(client, DEFAULT_CONFIG, _REPO)
        assert result.branch_name is not None
        assert result.branch_name.startswith("blinklabs/standardize-ci-")
        assert not hasattr(result, "pull_request_url")

    def test_writes_all_changed_files(self):
        client = FakeRepositoryClient([])
        reconcile_repository(client, DEFAULT_CONFIG, _REPO)
        written_paths = {w["path"] for w in client.writes}
        assert written_paths == {defn.target_path for defn in DEFAULT_CONFIG.workflows}

    def test_preserves_sha_for_updated_file(self):
        """When a file exists at the target path the old SHA must be forwarded."""
        old_content = (
            "name: Test Issue Close Trigger\n"
            "on:\n  issues:\n    types:\n      - closed\n"
            "jobs:\n  test:\n    runs-on: ubuntu-latest\n"
        )
        client = FakeRepositoryClient(
            [
                WorkflowFile(
                    path=".github/workflows/test-issue-on-close.yml",
                    sha="old-sha-xyz",
                    content=old_content,
                )
            ]
        )
        reconcile_repository(client, DEFAULT_CONFIG, _REPO)
        update_write = next(
            w for w in client.writes if "test-issue-on-close" in w["path"]
        )
        assert update_write["sha"] == "old-sha-xyz"

    def test_set_project_closed_date_workflow_generated(self):
        """set-project-closed-date definition generates output targeting its own path."""
        client = FakeRepositoryClient([])
        reconcile_repository(client, DEFAULT_CONFIG, _REPO)
        written_paths = {w["path"] for w in client.writes}
        assert ".github/workflows/set-project-closed-date.yml" in written_paths

    def test_set_project_closed_date_explicit_secrets(self):
        """set-project-closed-date must use explicit secret mapping, not inherit."""
        client = FakeRepositoryClient([])
        reconcile_repository(client, DEFAULT_CONFIG, _REPO)
        set_date_write = next(
            w for w in client.writes if "set-project-closed-date" in w["path"]
        )
        assert "project_pat:" in set_date_write["content"]
        assert "secrets: inherit" not in set_date_write["content"]

    def test_new_file_gets_none_sha(self):
        """Files that do not exist yet should be created with sha=None."""
        client = FakeRepositoryClient([])
        reconcile_repository(client, DEFAULT_CONFIG, _REPO)
        for write in client.writes:
            assert write["sha"] is None

    def test_skips_central_actions_repo(self):
        client = FakeRepositoryClient([])
        central = RepositoryRef(
            owner="blinklabs-io", repo="actions", default_branch="main"
        )
        result = reconcile_repository(client, DEFAULT_CONFIG, central)
        assert result.skipped_reason == "central-actions-repository"
        assert client.writes == []

    def test_dry_run_does_not_write(self):
        client = FakeRepositoryClient([])
        result = reconcile_repository(client, DEFAULT_CONFIG, _REPO, dry_run=True)
        assert result.skipped_reason == "dry-run"
        assert client.writes == []
        assert len(result.changed_files) == len(DEFAULT_CONFIG.workflows)

    def test_branch_created_with_date_suffix(self):
        client = FakeRepositoryClient([])
        result = reconcile_repository(client, DEFAULT_CONFIG, _REPO)
        assert len(client._branches) == 1
        assert client._branches[0].startswith("blinklabs/standardize-ci-")
        assert result.branch_name == client._branches[0]
