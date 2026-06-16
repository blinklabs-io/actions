"""Repository reconciliation logic.

The reconciler is decoupled from github3.py via the GitHubRepositoryClient
Protocol so it can be tested with a simple fake implementation.
"""

from __future__ import annotations

from datetime import date
from typing import Optional, Protocol, runtime_checkable

from .types import (
    ExistingWorkflowMatch,
    ReconcileResult,
    RepositoryRef,
    StandardizerConfig,
    WorkflowFile,
)
from .workflow_templates import (
    find_workflow_for_definition,
    generate_standard_workflow,
    is_standardized_workflow,
)


@runtime_checkable
class GitHubRepositoryClient(Protocol):
    def list_workflow_files(self, repository: RepositoryRef) -> list[WorkflowFile]: ...
    def get_default_branch_sha(self, repository: RepositoryRef) -> str: ...
    def ensure_branch(self, repository: RepositoryRef, branch: str, sha: str) -> None: ...
    def upsert_file(
        self,
        repository: RepositoryRef,
        branch: str,
        path: str,
        content: str,
        message: str,
        sha: Optional[str] = None,
    ) -> None: ...


def reconcile_repository(
    client: GitHubRepositoryClient,
    config: StandardizerConfig,
    repository: RepositoryRef,
    dry_run: bool = False,
) -> ReconcileResult:
    """Reconcile one repository against the central workflow templates.

    Returns a ReconcileResult describing what changed (or was skipped).
    When *dry_run* is True the function computes the diff but makes no
    API writes and opens no pull request.
    """
    # Never open pull requests against the central actions repo itself.
    if repository.repo == config.central_repo and repository.owner == config.org:
        return ReconcileResult(
            repository=repository,
            changed_files=[],
            skipped_reason="central-actions-repository",
        )

    workflow_files = client.list_workflow_files(repository)

    matches: list[ExistingWorkflowMatch] = []
    for definition in config.workflows:
        existing_file = find_workflow_for_definition(workflow_files, definition)
        generated_content = generate_standard_workflow(definition, config)
        changed = (
            existing_file is None
            or not is_standardized_workflow(existing_file.content, definition, config)
            or existing_file.content != generated_content
        )
        matches.append(
            ExistingWorkflowMatch(
                definition=definition,
                existing_file=existing_file,
                generated_content=generated_content,
                changed=changed,
            )
        )

    changed_matches = [m for m in matches if m.changed]
    if not changed_matches:
        return ReconcileResult(repository=repository, changed_files=[])

    changed_files = [m.definition.target_path for m in changed_matches]

    if dry_run:
        return ReconcileResult(
            repository=repository,
            changed_files=changed_files,
            skipped_reason="dry-run",
        )

    base_sha = client.get_default_branch_sha(repository)
    branch_name = f"{config.branch_prefix}-{date.today().isoformat()}"
    client.ensure_branch(repository, branch_name, base_sha)

    for match in changed_matches:
        # Re-use the existing file's SHA only when the path is unchanged so
        # the update call targets the right blob.
        existing_sha: Optional[str] = None
        if (
            match.existing_file is not None
            and match.existing_file.path == match.definition.target_path
        ):
            existing_sha = match.existing_file.sha

        client.upsert_file(
            repository,
            branch_name,
            match.definition.target_path,
            match.generated_content,
            config.commit_message,
            existing_sha,
        )

    # Files have been committed to the branch. Pull request creation is
    # intentionally left to the repository owner so changes can be reviewed
    # before merging into the default branch.
    return ReconcileResult(
        repository=repository,
        changed_files=changed_files,
        branch_name=branch_name,
    )
