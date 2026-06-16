"""CLI entry point for manual / scheduled reconciliation runs."""

from __future__ import annotations

import argparse
import os
import sys

from .config import DEFAULT_CONFIG
from .github_client import (
    OctokitRepositoryClient,
    create_github_app,
    get_installation_client,
    list_installation_repositories,
)
from .reconciler import reconcile_repository


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Reconcile Blink Labs repository CI workflows."
    )
    sub = parser.add_subparsers(dest="command", required=True)

    reconcile_cmd = sub.add_parser("reconcile", help="Reconcile one or all repositories.")
    reconcile_cmd.add_argument(
        "--installation-id",
        type=int,
        required=True,
        help="GitHub App installation ID.",
    )
    reconcile_cmd.add_argument(
        "--repo",
        metavar="NAME",
        help="Limit reconciliation to a single repository name.",
    )
    reconcile_cmd.add_argument(
        "--dry-run",
        action="store_true",
        default=os.environ.get("DRY_RUN", "false").lower() == "true",
        help="Print what would change without writing anything.",
    )

    args = parser.parse_args()

    app = create_github_app()
    gh = get_installation_client(app, args.installation_id)
    repositories = list_installation_repositories(gh)

    if args.repo:
        repositories = [r for r in repositories if r.repo == args.repo]

    if not repositories:
        print(
            f"No matching repositories found for installation {args.installation_id}.",
            file=sys.stderr,
        )
        sys.exit(1)

    client = OctokitRepositoryClient(gh)
    exit_code = 0

    for repository in repositories:
        try:
            result = reconcile_repository(
                client, DEFAULT_CONFIG, repository, dry_run=args.dry_run
            )
            name = f"{result.repository.owner}/{result.repository.repo}"
            if result.skipped_reason:
                print(f"{name}: skipped ({result.skipped_reason})")
            elif not result.changed_files:
                print(f"{name}: already standardized")
            else:
                branch_part = (
                    f" → branch: {result.branch_name}" if result.branch_name else ""
                )
                print(f"{name}: updated {', '.join(result.changed_files)}{branch_part}")
                print(f"  Open a pull request manually from branch '{result.branch_name}'.")
        except Exception as exc:  # noqa: BLE001
            print(
                f"{repository.owner}/{repository.repo}: error — {exc}", file=sys.stderr
            )
            exit_code = 1

    sys.exit(exit_code)
