"""HTTP webhook server.

Uses only the Python standard library for the HTTP layer so no additional web
framework dependency is needed beyond the three required libraries.

Webhook payloads are verified with HMAC-SHA256 before being dispatched.
Event handlers run in daemon threads so the 202 response is returned to
GitHub immediately.
"""

from __future__ import annotations

import hashlib
import hmac
import json
import os
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import Any

from .config import DEFAULT_CONFIG
from .github_client import (
    OctokitRepositoryClient,
    create_github_app,
    get_installation_client,
)
from .reconciler import reconcile_repository
from .types import RepositoryRef

_DRY_RUN: bool = os.environ.get("DRY_RUN", "false").lower() == "true"
_app: Any = None  # github3.GitHubApp, set at start_server()


# ---------------------------------------------------------------------------
# Signature verification
# ---------------------------------------------------------------------------


def _verify_signature(body: bytes, signature: str) -> bool:
    secret = os.environ.get("GITHUB_WEBHOOK_SECRET", "")
    if not secret:
        return False
    expected = hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(f"sha256={expected}", signature)


# ---------------------------------------------------------------------------
# Request handler
# ---------------------------------------------------------------------------


class _WebhookHandler(BaseHTTPRequestHandler):
    def do_GET(self) -> None:
        if self.path == "/healthz":
            self._send_json(200, {"ok": True})
        else:
            self._send_json(404, {"error": "not found"})

    def do_POST(self) -> None:
        if self.path == "/api/github/webhooks":
            self._handle_webhook()
        else:
            self._send_json(404, {"error": "not found"})

    def _handle_webhook(self) -> None:
        content_length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(content_length)

        signature = self.headers.get("x-hub-signature-256", "")
        event_name = self.headers.get("x-github-event", "")
        delivery_id = self.headers.get("x-github-delivery", "")

        if not delivery_id or not event_name:
            self._send_json(400, {"error": "Missing required GitHub webhook headers."})
            return

        if not _verify_signature(body, signature):
            self._send_json(400, {"error": "Invalid webhook signature."})
            return

        try:
            payload: dict = json.loads(body)
        except json.JSONDecodeError:
            self._send_json(400, {"error": "Invalid JSON payload."})
            return

        # Acknowledge immediately, then process asynchronously.
        self._send_json(202, {"accepted": True})
        threading.Thread(
            target=_dispatch_event,
            args=(event_name, payload),
            daemon=True,
        ).start()

    def _send_json(self, status: int, data: dict[str, Any]) -> None:
        body = json.dumps(data).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, fmt: str, *args: Any) -> None:
        print(f"[{self.address_string()}] {fmt % args}")


# ---------------------------------------------------------------------------
# Event dispatch
# ---------------------------------------------------------------------------


def _dispatch_event(event_name: str, payload: dict) -> None:
    if _app is None:
        return
    if event_name == "push":
        _handle_push(payload)
    elif event_name == "installation_repositories":
        _handle_installation_repositories_added(payload)


def _handle_push(payload: dict) -> None:
    repo_data: dict = payload.get("repository", {})
    if repo_data.get("archived") or repo_data.get("disabled"):
        return

    installation_id = (payload.get("installation") or {}).get("id")
    if not installation_id:
        return

    repository = RepositoryRef(
        owner=repo_data["owner"]["login"],
        repo=repo_data["name"],
        default_branch=repo_data.get("default_branch", "main"),
    )

    try:
        gh = get_installation_client(_app, installation_id)
        client = OctokitRepositoryClient(gh)
        result = reconcile_repository(client, DEFAULT_CONFIG, repository, dry_run=_DRY_RUN)
        if result.changed_files:
            print(
                f"Reconciled {repository.owner}/{repository.repo}: "
                f"{', '.join(result.changed_files)} — "
                f"open a PR from branch '{result.branch_name}'"
            )
    except Exception as exc:  # noqa: BLE001
        print(f"Error reconciling {repository.owner}/{repository.repo}: {exc}")


def _handle_installation_repositories_added(payload: dict) -> None:
    installation: dict = payload.get("installation", {})
    installation_id = installation.get("id")
    if not installation_id:
        return

    owner: str = (installation.get("account") or {}).get("login", DEFAULT_CONFIG.org)
    repositories_added: list[dict] = payload.get("repositories_added", [])

    try:
        gh = get_installation_client(_app, installation_id)
        client = OctokitRepositoryClient(gh)
        for repo_data in repositories_added:
            repository = RepositoryRef(
                owner=owner,
                repo=repo_data["name"],
                default_branch=repo_data.get("default_branch", "main"),
            )
            result = reconcile_repository(
                client, DEFAULT_CONFIG, repository, dry_run=_DRY_RUN
            )
            if result.changed_files:
                print(
                    f"Reconciled {repository.owner}/{repository.repo}: "
                    f"{', '.join(result.changed_files)}"
                )
    except Exception as exc:  # noqa: BLE001
        print(f"Error handling installation_repositories.added: {exc}")


# ---------------------------------------------------------------------------
# Server entry point
# ---------------------------------------------------------------------------


def start_server(port: int = 3000) -> None:
    global _app
    _app = create_github_app()
    server = HTTPServer(("", port), _WebhookHandler)
    print(f"Blink Labs CI standardizer listening on :{port}")
    server.serve_forever()
