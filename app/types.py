"""Domain types for the CI workflow standardizer."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Optional, Union


@dataclass
class WorkflowDefinition:
    """Describes one standard CI workflow and how to detect / generate it."""

    id: str
    name: str
    target_path: str
    match_file_names: list[str]
    match_name_includes: list[str]
    central_workflow_path: str
    job_name: str
    trigger: str
    permissions: dict[str, str] = field(default_factory=dict)
    with_inputs: dict[str, Any] = field(default_factory=dict)
    secrets: Union[str, dict[str, str]] = "inherit"


@dataclass
class StandardizerConfig:
    """Top-level configuration for the reconciler."""

    org: str
    central_repo: str
    central_ref: str
    branch_prefix: str
    commit_message: str
    workflows: list[WorkflowDefinition]


@dataclass
class RepositoryRef:
    owner: str
    repo: str
    default_branch: str


@dataclass
class WorkflowFile:
    path: str
    content: str
    sha: str


@dataclass
class ExistingWorkflowMatch:
    definition: WorkflowDefinition
    existing_file: Optional[WorkflowFile]
    generated_content: str
    changed: bool


@dataclass
class ReconcileResult:
    repository: RepositoryRef
    changed_files: list[str]
    branch_name: Optional[str] = None
    skipped_reason: Optional[str] = None
