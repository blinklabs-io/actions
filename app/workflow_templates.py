"""Workflow YAML generation and detection utilities.

PyYAML is used to *parse* existing workflow files.
Generation is done with string formatting to avoid PyYAML's
well-known 'on' key quoting quirk.
"""

from __future__ import annotations

from typing import Any, Optional

import yaml

from .types import StandardizerConfig, WorkflowDefinition, WorkflowFile


def generate_standard_workflow(
    definition: WorkflowDefinition, config: StandardizerConfig
) -> str:
    """Return a workflow file body that calls the central reusable workflow."""
    lines: list[str] = [
        f"name: {definition.name}",
        "",
        definition.trigger,
        "",
        "jobs:",
        f"  {definition.job_name}:",
        f"    uses: {config.org}/{config.central_repo}/{definition.central_workflow_path}@{config.central_ref}",
    ]

    if definition.permissions:
        lines.append("    permissions:")
        for key, value in definition.permissions.items():
            lines.append(f"      {key}: {value}")

    if definition.with_inputs:
        lines.append("    with:")
        for key, value in definition.with_inputs.items():
            lines.append(f"      {key}: {_format_value(value)}")

    if definition.secrets == "inherit":
        lines.append("    secrets: inherit")
    elif isinstance(definition.secrets, dict) and definition.secrets:
        lines.append("    secrets:")
        for key, value in definition.secrets.items():
            lines.append(f"      {key}: {value}")

    return "\n".join(lines) + "\n"


def is_standardized_workflow(
    content: str, definition: WorkflowDefinition, config: StandardizerConfig
) -> bool:
    """Return True when the file already calls the central reusable workflow."""
    marker = (
        f"uses: {config.org}/{config.central_repo}/"
        f"{definition.central_workflow_path}@{config.central_ref}"
    )
    return marker in content


def find_workflow_for_definition(
    files: list[WorkflowFile], definition: WorkflowDefinition
) -> Optional[WorkflowFile]:
    """Find the best matching file for a workflow definition.

    Resolution order:
      1. Exact target path match.
      2. File name (basename) match against known aliases.
      3. Workflow display-name (parsed from YAML) substring match.
    """
    for f in files:
        if f.path == definition.target_path:
            return f

    for f in files:
        basename = f.path.split("/")[-1].lower()
        if basename in definition.match_file_names:
            return f

    for f in files:
        workflow_name = read_workflow_name(f.content)
        if any(candidate in workflow_name for candidate in definition.match_name_includes):
            return f

    return None


def read_workflow_name(content: str) -> str:
    """Return the lowercase workflow display name from YAML, or '' on error."""
    try:
        data = yaml.safe_load(content)
        if isinstance(data, dict) and isinstance(data.get("name"), str):
            return data["name"].lower()
    except yaml.YAMLError:
        pass
    return ""


# ---------------------------------------------------------------------------
# Internal helpers
# ---------------------------------------------------------------------------


def _format_value(value: Any) -> str:
    """Serialize a workflow input value to a YAML-safe inline string."""
    if isinstance(value, bool):
        return str(value).lower()
    if isinstance(value, (int, float)):
        return str(value)
    if isinstance(value, str):
        # GitHub Actions expression — pass through verbatim
        if "${{" in value:
            return value
        # Safe alphanumeric-ish value — no quoting needed
        if all(c.isalnum() or c in "_./-:" for c in value):
            return value
    # Fall back to PyYAML's flow scalar (single-line) representation
    return yaml.dump(value, default_flow_style=True).strip()
