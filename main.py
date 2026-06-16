"""Top-level entry point.

Usage:
  python main.py serve       # Start the webhook HTTP server (default)
  python main.py reconcile   # Run the CLI reconciler (pass -- --help for flags)
"""

from __future__ import annotations

import os
import sys


def _load_env(path: str = ".env") -> None:
    """Minimal .env loader — does not override already-set variables."""
    if not os.path.isfile(path):
        return
    with open(path) as fh:
        for line in fh:
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            key, _, value = line.partition("=")
            key = key.strip()
            value = value.strip().strip('"').strip("'")
            if key not in os.environ:
                os.environ[key] = value


def main() -> None:
    _load_env()
    command = sys.argv[1] if len(sys.argv) > 1 else "serve"

    if command == "serve":
        from app.server import start_server

        port = int(os.environ.get("APP_PORT", "3000"))
        start_server(port)

    elif command == "reconcile":
        # Remove "reconcile" from argv so argparse inside cli.main() sees the flags.
        sys.argv = [sys.argv[0]] + sys.argv[1:]
        from app.cli import main as cli_main

        cli_main()

    else:
        print(f"Unknown command: {command!r}", file=sys.stderr)
        print("Usage: python main.py [serve|reconcile]", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()
