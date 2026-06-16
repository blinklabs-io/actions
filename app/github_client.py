"""GitHub API client built on github3.py.

GitPython is used to inspect the local actions repository so the server can
discover which central workflow templates are available without making API
calls to GitHub.
"""

from __future__ import annotations

import os
from typing import Optional

import github3
import github3.exceptions
from git import InvalidGitRepositoryError, Repo

from .types import RepositoryRef, WorkflowFile


# ---------------------------------------------------------------------------
# GitHub App authentication
# ---------------------------------------------------------------------------


def create_github_app() -> github3.GitHubApp:
    """Build a GitHubApp from environment variables."""
    app_id = os.environ.get("GITHUB_APP_ID", "")
    private_key = os.environ.get("GITHUB_PRIVATE_KEY", "").replace("\\n", "\n")

    if not app_id or not private_key:
        raise EnvironmentError(
            "GITHUB_APP_ID and GITHUB_PRIVATE_KEY environment variables are required."
        )

    return github3.GitHubApp(app_id=int(app_id), private_key=private_key.encode())


def get_installation_client(
    app: github3.GitHubApp, installation_id: int
) -> github3.GitHub:
    """Return an authenticated GitHub client for the given installation."""
    token_response = app.installation_token(installation_id)
    # github3.py returns a dict-like object; normalise to a plain string.
    if isinstance(token_response, dict):
        token: str = token_response["token"]
    else:
        token = token_response.token  # type: ignore[union-attr]

    return github3.login(token=token)


def list_installation_repositories(gh: github3.GitHub) -> list[RepositoryRef]:
    """Return all repositories accessible to this installation token.

    Uses the /installation/repositories endpoint via the authenticated session
    so no extra permission is required beyond the app installation itself.
    """
    repos: list[RepositoryRef] = []
    page = 1
    while True:
        response = gh.session.get(
            "https://api.github.com/installation/repositories",
            params={"per_page": 100, "page": page},
        )
        if not response.ok:
            break
        data = response.json()
        batch: list[dict] = data.get("repositories", [])
        if not batch:
            break
        for repo in batch:
            repos.append(
                RepositoryRef(
                    owner=repo["owner"]["login"],
                    repo=repo["name"],
                    default_branch=repo.get("default_branch", "main"),
                )
            )
        if len(batch) < 100:
            break
        page += 1

    return repos


# ---------------------------------------------------------------------------
# GitPython helpers — inspect the local actions repository
# ---------------------------------------------------------------------------


def list_central_workflow_templates(repo_path: str = ".") -> list[str]:
    """Use GitPython to list workflow template filenames in the local repo.

    Returns an empty list when the path is not a valid git repository or when
    the .github/workflows directory does not exist in HEAD.
    """
    try:
        repo = Repo(repo_path)
        workflows_tree = repo.head.commit.tree[".github/workflows"]
        return [
            blob.name
            for blob in workflows_tree.blobs
            if blob.name.endswith((".yml", ".yaml"))
        ]
    except (InvalidGitRepositoryError, KeyError):
        return []


def read_central_workflow_template(
    workflow_path: str, repo_path: str = "."
) -> Optional[str]:
    """Use GitPython to read a central workflow template from HEAD.

    Returns None when the file does not exist or the repo is invalid.
    """
    try:
        repo = Repo(repo_path)
        blob = repo.head.commit.tree[workflow_path]
        return blob.data_stream.read().decode("utf-8")
    except (InvalidGitRepositoryError, KeyError):
        return None


# ---------------------------------------------------------------------------
# github3.py repository client
# ---------------------------------------------------------------------------


class OctokitRepositoryClient:
    """Implements GitHubRepositoryClient using github3.py."""

    def __init__(self, gh: github3.GitHub) -> None:
        self._gh = gh

    # ------------------------------------------------------------------
    # Protocol methods
    # ------------------------------------------------------------------

    def list_workflow_files(self, repository: RepositoryRef) -> list[WorkflowFile]:
        repo = self._gh.repository(repository.owner, repository.repo)
        if repo is None:
            return []

        try:
            contents = repo.directory_contents(
                ".github/workflows", ref=repository.default_branch
            )
        except Exception:
            return []

        if not contents:
            return []

        # directory_contents returns dict[name, ShortContent] by default
        items = contents.values() if isinstance(contents, dict) else [c for _, c in contents]

        files: list[WorkflowFile] = []
        for item in items:
            if not item.name.lower().endswith((".yml", ".yaml")):
                continue
            try:
                file_obj = repo.file_contents(item.path, ref=repository.default_branch)
                if file_obj is not None and file_obj.decoded_content:
                    files.append(
                        WorkflowFile(
                            path=file_obj.path,
                            sha=file_obj.sha,
                            content=file_obj.decoded_content.decode("utf-8"),
                        )
                    )
            except Exception:
                continue

        return files

    def get_default_branch_sha(self, repository: RepositoryRef) -> str:
        repo = self._gh.repository(repository.owner, repository.repo)
        branch = repo.branch(repository.default_branch)
        return branch.commit.sha  # type: ignore[union-attr]

    def ensure_branch(
        self, repository: RepositoryRef, branch: str, sha: str
    ) -> None:
        repo = self._gh.repository(repository.owner, repository.repo)
        try:
            repo.create_ref(f"refs/heads/{branch}", sha)
        except github3.exceptions.UnprocessableEntity:
            # Branch already exists — nothing to do.
            pass

    def upsert_file(
        self,
        repository: RepositoryRef,
        branch: str,
        path: str,
        content: str,
        message: str,
        sha: Optional[str] = None,
    ) -> None:
        repo = self._gh.repository(repository.owner, repository.repo)
        # github3.py accepts raw bytes and handles base64 encoding internally.
        content_bytes = content.encode("utf-8")

        existing_sha = sha
        if existing_sha is None:
            # Check whether the file already exists on the target branch.
            try:
                existing_file = repo.file_contents(path, ref=branch)
                if existing_file is not None:
                    existing_sha = existing_file.sha
            except Exception:
                existing_sha = None

        if existing_sha:
            repo.update_file(path, message, content_bytes, existing_sha, branch=branch)
        else:
            repo.create_file(path, message, content_bytes, branch=branch)

    def find_open_pull_request(
        self, repository: RepositoryRef, branch: str
    ) -> Optional[dict]:
        repo = self._gh.repository(repository.owner, repository.repo)
        head_filter = f"{repository.owner}:{branch}"
        for pr in repo.pull_requests(state="open", head=head_filter):
            return {"number": pr.number, "html_url": pr.html_url}
        return None

    def create_pull_request(
        self, repository: RepositoryRef, branch: str, title: str, body: str
    ) -> str:
        repo = self._gh.repository(repository.owner, repository.repo)
        pr = repo.create_pull(title, repository.default_branch, branch, body=body)
        return pr.html_url  # type: ignore[union-attr]
