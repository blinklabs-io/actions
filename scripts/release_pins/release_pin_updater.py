"""Find published chart and image versions and prepare one PR per Helmfile release."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import subprocess
import sys
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Protocol, TypedDict

HELMFILE = Path("helmfile-app/helmfile-demeter.yaml.gotmpl")
REPOS = Path("helmfile-app/helmfile-repos.yaml")
MARKER = "<!-- release-pin-updater: {} -->"
SEMVER = re.compile(r"^(v?)(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-(\d+))?$")
RELEASE = re.compile(r"^  - name: (.+?)\s*$")
FIELD = re.compile(r"^    (chart|version):\s*(\S+)\s*$")
VALUE_FILE = re.compile(r"^      - ([\w./-]+\.ya?ml(?:\.gotmpl)?)\s*$")
KEY = re.compile(r"^(\s*)([\w-]+):(?:\s*(.*))?$")
RANGE = re.compile(r"^\s*\{\{-?\s*range \$(\w+) := list((?:\s+\"[\w.-]+\")+)\s*-?\}\}\s*$")
ACTION = re.compile(r"\{\{-?\s*(if|range|with|define|block|end)\b")

# These charts expose an image tag override but obtain the repository from the
# chart default or template. The mappings are checked against published charts.
IMAGE_SOURCES: dict[str, str] = {
    "helmfile-app/butane-oracle/values.production.yaml.gotmpl": "sundaeswap/butane-oracle",
    "helmfile-app/butane-oracle/values.test.yaml.gotmpl": "sundaeswap/butane-oracle",
    "helmfile-app/dolos/values.mainnet.yaml.gotmpl": "ghcr.io/txpipe/dolos",
    "helmfile-app/dolos/values.preprod.yaml.gotmpl": "ghcr.io/txpipe/dolos",
    "helmfile-app/dolos/values.preview.yaml.gotmpl": "ghcr.io/txpipe/dolos",
    "helmfile-app/grafana-pdc/values.yaml.gotmpl": "grafana/pdc-agent",
    "helmfile-app/grafana-pdc/values.demeter.global.gotmpl": "grafana/pdc-agent",
    "helmfile-app/tosidrop/values.mainnet.yaml.gotmpl": "ghcr.io/tosidrop/vm-frontend",
    "helmfile-app/tracing/tempo.yaml": "docker.io/grafana/tempo",
}


@dataclass(frozen=True)
class ReleasePin:
    release: str
    kind: str
    path: str
    ordinal: int
    current: str
    target: str
    source: str
    digest: str = ""


@dataclass(frozen=True)
class ReleaseSpec:
    name: str
    chart: str
    version: str
    values: tuple[str, ...]
    chart_owner: str = ""


class IssueReference(TypedDict):
    number: int


class OpenIssue(IssueReference):
    title: str
    body: str
    author: PRAuthor


class PublishedPR(IssueReference):
    headRefName: str
    body: str
    url: str


class PRAuthor(TypedDict):
    login: str


class OpenPR(PublishedPR):
    baseRefName: str
    isCrossRepository: bool
    author: PRAuthor
    title: str


class ClosedPR(IssueReference):
    body: str
    mergedAt: str | None
    url: str


def run(*args: str, cwd: Path | None = None) -> str:
    result = subprocess.run(args, cwd=cwd, text=True, capture_output=True, check=False)
    if result.returncode:
        raise RuntimeError(f"{' '.join(args[:3])} failed: {result.stderr.strip()[:600]}")
    return result.stdout.strip()


def semver(value: str) -> tuple[str, tuple[int, int, int, int], bool] | None:
    match = SEMVER.fullmatch(value)
    if not match:
        return None
    prefix, major, minor, patch, build = match.groups()
    return prefix, (int(major), int(minor), int(patch), int(build or 0)), build is not None


def newest(current: str, tags: list[str]) -> str | None:
    parsed = semver(current)
    if parsed is None:
        return None
    prefix, current_key, has_build = parsed
    eligible = [(part[1], tag) for tag in tags if (part := semver(tag)) and part[0] == prefix and part[2] == has_build and part[1] > current_key]
    return max(eligible, key=lambda item: item[0])[1] if eligible else None


def scalar(line: str) -> str | None:
    match = KEY.match(line)
    if match is None or match.group(3) is None:
        return None
    value = match.group(3).split(" #", 1)[0].strip()
    if len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
        value = value[1:-1]
    return value if value and "{{" not in value and "}}" not in value else None


def replace_scalar(line: str, target: str) -> str:
    match = KEY.match(line)
    if match is None or match.group(3) is None:
        raise ValueError(f"cannot replace scalar: {line}")
    raw = match.group(3)
    value_part, separator, comment = raw.partition(" #")
    quote = value_part.strip()[:1] if value_part.strip()[:1] in "\"'" else ""
    rendered = f"{quote}{target}{quote}" if quote else target
    prefix = line[: line.index(raw)]
    return prefix + rendered + (separator + comment if separator else "")


def release_blocks(lines: list[str]) -> list[tuple[int, int, str, tuple[str, str] | None]]:
    """Return (start, end, name, range binding) per release, expanding literal range lists."""
    starts = [(i, match.group(1)) for i, line in enumerate(lines) if (match := RELEASE.match(line))]
    ranges: list[tuple[int, int, str, list[str]]] = []
    for i, line in enumerate(lines):
        if match := RANGE.match(line):
            end = matching_end(lines, i)
            ranges.append((i, end, match.group(1), re.findall(r'"([^"]+)"', match.group(2))))
    blocks: list[tuple[int, int, str, tuple[str, str] | None]] = []
    for position, (start, name) in enumerate(starts):
        end = starts[position + 1][0] if position + 1 < len(starts) else len(lines)
        ranged = next((item for item in ranges if item[0] < start < item[1]), None)
        if ranged is None:
            blocks.append((start, end, name, None))
            continue
        end = min(end, ranged[1])
        for value in ranged[3]:
            blocks.append((start, end, substitute(name, ranged[2], value), (ranged[2], value)))
    return blocks


def matching_end(lines: list[str], start: int) -> int:
    """Return the line closing the action opened on lines[start], counting nested actions."""
    depth = 0
    for index in range(start, len(lines)):
        for keyword in ACTION.findall(lines[index]):
            depth += -1 if keyword == "end" else 1
            if depth == 0:
                return index
    return len(lines)


def substitute(text: str, variable: str, value: str) -> str:
    return re.sub(r"\{\{-?\s*\$" + re.escape(variable) + r"\s*-?\}\}", value, text)


def releases(root: Path) -> tuple[list[ReleaseSpec], list[str]]:
    lines = (root / HELMFILE).read_text().splitlines()
    found: list[ReleaseSpec] = []
    skipped: list[str] = []
    owners: dict[int, str] = {}
    for start, end, name, binding in release_blocks(lines):
        block = lines[start:end]
        if binding:
            block = [substitute(line, *binding) for line in block]
        fields = {match.group(1): match.group(2) for line in block if (match := FIELD.match(line))}
        if "{{" in name or "chart" not in fields or "{{" in fields["chart"]:
            skipped.append(f"{name}: dynamic release name or chart")
            continue
        values = tuple(match.group(1) for line in block if (match := VALUE_FILE.match(line)))
        owner = owners.setdefault(start, name) if binding else name
        found.append(ReleaseSpec(name, fields["chart"], fields.get("version", ""), values,
                                 owner if owner != name else ""))
    return found, skipped


def repo_urls(root: Path) -> dict[str, str]:
    lines = (root / REPOS).read_text().splitlines()
    urls: dict[str, str] = {}
    name = ""
    for line in lines:
        if match := re.match(r"^  - name: ([\w-]+)$", line):
            name = match.group(1)
        elif name and (match := re.match(r"^    url: (https://\S+)$", line)):
            urls[name] = match.group(1)
    return urls


def yaml_fields(path: Path) -> list[tuple[int, str, str, tuple[str, ...]]]:
    """Return simple mapping keys and their parent paths without rendering templates."""
    stack: list[tuple[int, str]] = []
    fields: list[tuple[int, str, str, tuple[str, ...]]] = []
    for index, line in enumerate(path.read_text().splitlines()):
        match = KEY.match(line)
        if not match:
            continue
        indent, key, raw = match.groups()
        while stack and stack[-1][0] >= len(indent):
            stack.pop()
        parents = tuple(item[1] for item in stack)
        fields.append((index, key, scalar(line) or "", parents))
        if raw is None or not raw.strip():
            stack.append((len(indent), key))
    return fields


def image_entries(root: Path, relative: str) -> list[tuple[int, str, str, int, str]]:
    """Return (ordinal, tag, repository, tag line, digest) for static image pins."""
    path = root / relative
    fields = yaml_fields(path)
    repositories = {parents: value for _, key, value, parents in fields if key == "repository" and value}
    digests = {parents: value for _, key, value, parents in fields if key == "digest" and value}
    entries: list[tuple[int, str, str, int, str]] = []
    ordinal = 0
    for line, key, value, parents in fields:
        if key != "tag":
            continue
        source = repositories.get(parents, IMAGE_SOURCES.get(relative, ""))
        if "image" not in parents and relative not in IMAGE_SOURCES:
            continue
        entries.append((ordinal, value, source, line, digests.get(parents, "")))
        ordinal += 1
    return entries


def value_paths(spec: ReleaseSpec) -> tuple[str, ...]:
    paths = tuple("helmfile-app/" + value for value in spec.values)
    if spec.chart.startswith("charts/"):
        paths += ("helmfile-app/" + spec.chart + "/values.yaml",)
    return paths


class RegistryClient(Protocol):
    def versions(self, source: str) -> list[str]: ...
    def digest(self, reference: str) -> str: ...
    def verify(self, pin: ReleasePin) -> None: ...


class Registry:
    def __init__(self, root: Path) -> None:
        self.urls: dict[str, str] = repo_urls(root)
        self.tags: dict[str, list[str]] = {}
        self.prepared: set[str] = set()
        self.checked: int = 0

    def prepare_helm(self, name: str) -> None:
        if name in self.prepared:
            return
        run("helm", "repo", "add", name, self.urls[name], "--force-update")
        run("helm", "repo", "update", name)
        self.prepared.add(name)

    def versions(self, source: str) -> list[str]:
        if source in self.tags:
            return self.tags[source]
        if source.startswith("oci://"):
            tags = json.loads(run("skopeo", "list-tags", "docker://" + source.removeprefix("oci://")))["Tags"]
        elif "/" in source and source.split("/", 1)[0] in self.urls:
            self.prepare_helm(source.split("/", 1)[0])
            items = json.loads(run("helm", "search", "repo", source, "--versions", "--output", "json"))
            tags = [item["version"] for item in items if item.get("name") == source]
        else:
            tags = json.loads(run("skopeo", "list-tags", "docker://" + source))["Tags"]
        self.tags[source] = tags
        self.checked += 1
        return tags

    def digest(self, reference: str) -> str:
        # Hash the original manifest bytes, including any trailing newline, so
        # multi-platform images retain the index digest instead of a platform digest.
        result = subprocess.run(
            ("skopeo", "inspect", "--raw", "docker://" + reference),
            capture_output=True, check=True,
        )
        return "sha256:" + hashlib.sha256(result.stdout).hexdigest()

    def verify(self, pin: ReleasePin) -> None:
        if pin.kind == "chart" and not pin.source.startswith("oci://"):
            run("helm", "show", "chart", pin.source, "--version", pin.target)
        else:
            reference = pin.source.removeprefix("oci://") + ":" + pin.target
            digest = self.digest(reference)
            if pin.digest and digest != pin.digest:
                raise RuntimeError(f"digest changed while checking {reference}")


def plan(root: Path, registry: RegistryClient) -> tuple[dict[str, list[ReleasePin]], list[str]]:
    specs, skipped = releases(root)
    users: dict[str, set[str]] = {}
    for spec in specs:
        for relative in value_paths(spec):
            users.setdefault(relative, set()).add(spec.name)
    result: dict[str, list[ReleasePin]] = {}
    for spec in specs:
        pins: list[ReleasePin] = []
        if spec.chart_owner:
            # Expanded releases share one version line; only the first bumps it.
            skipped.append(f"{spec.name}: chart version shared with {spec.chart_owner}")
        elif spec.version and not spec.chart.startswith("charts/") and not spec.chart.startswith("../"):
            try:
                candidate = newest(spec.version, registry.versions(spec.chart))
                if candidate:
                    pin = ReleasePin(spec.name, "chart", str(HELMFILE), 0, spec.version, candidate, spec.chart)
                    registry.verify(pin)
                    pins.append(pin)
            except (OSError, RuntimeError, ValueError, subprocess.CalledProcessError) as error:
                skipped.append(f"{spec.name}: chart check failed ({error})")
        for relative in value_paths(spec):
            path = root / relative
            if not path.is_file() or path.name == "secrets.yaml":
                skipped.append(f"{spec.name}: values file unavailable for static scan: {relative}")
                continue
            if len(users[relative]) > 1:
                skipped.append(f"{spec.name}: shared values file needs coordinated update: {relative}")
                continue
            if any(key == "image" and item for _, key, item, _ in yaml_fields(path)):
                skipped.append(f"{spec.name}: direct image reference needs manual source mapping: {relative}")
            for ordinal, current, source, _, old_digest in image_entries(root, relative):
                if not source or not semver(current):
                    skipped.append(f"{spec.name}: image pin cannot be resolved safely: {relative}")
                    continue
                try:
                    candidate = newest(current, registry.versions(source))
                    if candidate:
                        digest = registry.digest(source + ":" + candidate) if old_digest else ""
                        pin = ReleasePin(spec.name, "image", relative, ordinal, current, candidate, source, digest)
                        registry.verify(pin)
                        pins.append(pin)
                except (OSError, RuntimeError, ValueError, subprocess.CalledProcessError) as error:
                    skipped.append(f"{spec.name}: image check failed for {relative} ({error})")
        if pins:
            result[spec.name] = pins
    return result, skipped


def apply_pins(root: Path, release: str, pins: list[ReleasePin]) -> list[str]:
    changed: list[str] = []
    specs, _ = releases(root)
    spec = next((item for item in specs if item.name == release), None)
    if spec is None:
        raise RuntimeError(f"release disappeared from branch: {release}")
    for pin in pins:
        if pin.kind == "chart" and spec.chart != pin.source:
            raise RuntimeError(f"chart source changed on branch: {release}")
        if pin.kind == "image" and pin.path not in value_paths(spec):
            raise RuntimeError(f"image values file changed on branch: {release}")
        path = root / pin.path
        lines = path.read_text().splitlines(keepends=True)
        if pin.kind == "chart":
            blocks = release_blocks([line.rstrip("\n") for line in lines])
            index, end = next((i, j) for i, j, name, _ in blocks if name == release)
            candidates = [i for i in range(index, end) if re.match(r"^    version:", lines[i])]
            if len(candidates) != 1:
                raise RuntimeError(f"chart version is ambiguous for {release}")
            index = candidates[0]
        else:
            entries = image_entries(root, pin.path)
            matches = [item for item in entries if item[0] == pin.ordinal]
            if len(matches) != 1:
                raise RuntimeError(f"image pin is ambiguous for {release}: {pin.path}")
            if matches[0][2] != pin.source:
                raise RuntimeError(f"image source changed on branch: {release}: {pin.path}")
            index = matches[0][3]
        current = scalar(lines[index].rstrip("\n"))
        current_version = semver(current) if current is not None else None
        target_version = semver(pin.target)
        if current_version is None or target_version is None:
            raise RuntimeError(f"pin changed to an unsupported form: {pin.path}")
        if current_version[1] >= target_version[1]:
            continue
        lines[index] = replace_scalar(lines[index].rstrip("\n"), pin.target) + "\n"
        if pin.kind == "image" and pin.digest:
            fields = yaml_fields(path)
            tag_field = next(item for item in fields if item[0] == index)
            digest_lines = [i for i, key, _, parents in fields if key == "digest" and parents == tag_field[3]]
            if len(digest_lines) != 1:
                raise RuntimeError(f"image digest is ambiguous for {release}: {pin.path}")
            digest_index = digest_lines[0]
            lines[digest_index] = replace_scalar(lines[digest_index].rstrip("\n"), pin.digest) + "\n"
        path.write_text("".join(lines))
        changed.append(pin.path)
    return sorted(set(changed))


def upstream_link(source: str, urls: dict[str, str]) -> str:
    alias = source.split("/", 1)[0]
    if alias in urls:
        return urls[alias]
    if source.startswith(("oci://ghcr.io/", "ghcr.io/")):
        parts = source.removeprefix("oci://ghcr.io/").removeprefix("ghcr.io/").split("/", 1)
        return f"https://github.com/orgs/{parts[0]}/packages/container/package/{parts[1].replace('/', '%2F')}" if len(parts) == 2 else source
    if source.startswith("docker.io/") or "." not in source.split("/", 1)[0]:
        name = source.removeprefix("docker.io/")
        return f"https://hub.docker.com/r/{name}/tags"
    return source


def description(release: str, pins: list[ReleasePin], urls: dict[str, str], issue: int | None = None) -> str:
    rows = "\n".join(f"| {pin.kind} | `{pin.current}` | `{pin.target}` | [upstream]({upstream_link(pin.source, urls)}) |" for pin in pins)
    body = f"{MARKER.format(release)}\n\nUpdate published pins for the `{release}` Helmfile release.\n\n| Pin | Current on main | Proposed | Source |\n| --- | --- | --- | --- |\n{rows}\n\nRegistry artifact existence is checked before this PR is opened. Normal PR checks validate the deployment diff. No deployment or merge is automatic.\n"
    return body + (f"\nCloses #{issue}\n" if issue else "")


def body_file(body: str) -> Path:
    with tempfile.NamedTemporaryFile(mode="w", suffix=".md", delete=False) as file:
        file.write(body)
        return Path(file.name)


def marker_release(body: str) -> str | None:
    match = re.search(r"<!-- release-pin-updater: ([\w.-]+) -->", body)
    return match.group(1) if match else None


def mentions_release(text: str, release: str) -> bool:
    return re.search(r"(?<![\w-])" + re.escape(release) + r"(?![\w-])", text, re.IGNORECASE) is not None


def publish(root: Path, repo: str, planned: dict[str, list[ReleasePin]], limit: int, urls: dict[str, str]) -> tuple[list[str], bool]:
    actor = os.environ["RELEASE_PIN_ACTOR"]
    open_prs: list[OpenPR] = json.loads(run("gh", "pr", "list", "--repo", repo, "--state", "open", "--limit", "500", "--json", "number,title,body,headRefName,baseRefName,isCrossRepository,author,url"))
    closed_prs: list[ClosedPR] = json.loads(run("gh", "pr", "list", "--repo", repo, "--state", "closed", "--limit", "500", "--json", "number,body,mergedAt,url"))
    open_issues: list[OpenIssue] = json.loads(run("gh", "issue", "list", "--repo", repo, "--state", "open", "--limit", "500", "--json", "number,title,body,author"))
    prs: dict[str, OpenPR] = {}
    unsafe: set[str] = set()
    for item in open_prs:
        release = marker_release(item["body"])
        if not release:
            continue
        match = re.search(r"(?m)^Closes #(\d+)$", item["body"])
        expected = f"chore/update-{re.sub(r'[^a-z0-9-]', '-', release.lower())}-issue-{match.group(1)}" if match else ""
        if (item["author"]["login"] != actor or item["isCrossRepository"] or
                item["baseRefName"] != "main" or item["headRefName"] != expected or
                release in prs):
            unsafe.add(release)
            continue
        prs[release] = item
    abandoned = {marker_release(item["body"]): item for item in closed_prs if marker_release(item["body"]) and not item["mergedAt"]}
    issues: dict[str, OpenIssue] = {}
    for item in open_issues:
        release = marker_release(item["body"] or "")
        if not release:
            continue
        if item["author"]["login"] != actor or release in issues:
            unsafe.add(release)
            continue
        issues[release] = item
    results: list[str] = []
    processed = 0
    failed = False
    for release, pins in sorted(planned.items()):
        if processed >= limit:
            break
        existing = prs.get(release)
        if release in unsafe:
            results.append(f"{release}: untrusted or duplicate updater PR marker needs manual review")
            failed = True
            continue
        if not existing and release in abandoned:
            results.append(f"{release}: closed unmerged updater PR needs manual review: {abandoned[release]['url']}")
            continue
        if not existing and any(mentions_release((item.get("title") or "") + "\n" + (item.get("body") or ""), release) for item in open_prs):
            results.append(f"{release}: skipped because another open PR tracks the release")
            continue
        if not existing and any(mentions_release(item["title"] + "\n" + (item["body"] or ""), release)
                                for item in open_issues if marker_release(item["body"] or "") != release):
            results.append(f"{release}: skipped because another open issue tracks the release")
            continue
        issue = issues.get(release)
        try:
            activity = publish_one(root, repo, release, pins, urls, existing, issue)
        except (OSError, RuntimeError, ValueError) as error:
            results.append(f"{release}: failed ({error})")
            failed = True
            continue
        if activity:
            results.append(activity)
            processed += 1
    return results, failed


def publish_one(root: Path, repo: str, release: str, pins: list[ReleasePin],
            urls: dict[str, str], existing: PublishedPR | None, issue: IssueReference | None) -> str | None:
    resume_branch = False
    if existing:
        match = re.search(r"(?m)^Closes #(\d+)$", existing["body"])
        if not match:
            raise RuntimeError("existing updater PR has no closing issue")
        number = int(match.group(1))
        branch = existing["headRefName"]
        run("git", "fetch", "origin", branch, cwd=root)
        start = "origin/" + branch
    else:
        if issue is None:
            issue_body = body_file(description(release, pins, urls))
            try:
                url = run("gh", "issue", "create", "--repo", repo, "--title", f"{release} release pins lag upstream", "--body-file", str(issue_body))
            finally:
                issue_body.unlink()
            number = int(url.rsplit("/", 1)[1])
        else:
            number = issue["number"]
        branch = f"chore/update-{re.sub(r'[^a-z0-9-]', '-', release.lower())}-issue-{number}"
        if issue and run("git", "ls-remote", "--heads", "origin", f"refs/heads/{branch}", cwd=root):
            run("git", "fetch", "origin", branch, cwd=root)
            verify_orphan_branch(root, branch, pins)
            start = "origin/" + branch
            resume_branch = True
        else:
            start = "origin/main"
    with tempfile.TemporaryDirectory(prefix="release-pin-") as temporary:
        worktree = Path(temporary) / "worktree"
        run("git", "worktree", "add", "--detach", str(worktree), start, cwd=root)
        try:
            if existing or resume_branch:
                run("git", "merge", "origin/main", "--signoff", "-m", f"chore: merge main into {branch}", cwd=worktree)
            changed = apply_pins(worktree, release, pins)
            if not changed and not existing and not resume_branch:
                raise RuntimeError(f"no changes produced for new release PR: {release}")
            if changed:
                run("git", "diff", "--check", cwd=worktree)
                run("git", "add", "--", *changed, cwd=worktree)
                run("git", "commit", "-s", "-m", f"chore: update {release} release pins", cwd=worktree)
            ahead = bool(existing or resume_branch) and run("git", "rev-list", "--count", f"origin/{branch}..HEAD", cwd=worktree) != "0"
            if changed or ahead:
                run("git", "push", "origin", f"HEAD:refs/heads/{branch}", cwd=worktree)
            if existing and not changed:
                desired = description(release, pins, urls, number)
                if existing["body"] != desired:
                    body = body_file(desired)
                    try:
                        run("gh", "pr", "edit", str(existing["number"]), "--repo", repo, "--body-file", str(body), cwd=worktree)
                    finally:
                        body.unlink()
                    return f"{release}: refreshed {existing['url']}"
                return f"{release}: updated {existing['url']}" if ahead else None
            body = body_file(description(release, pins, urls, number))
            try:
                if existing:
                    run("gh", "pr", "edit", str(existing["number"]), "--repo", repo, "--body-file", str(body), cwd=worktree)
                    activity = f"{release}: updated {existing['url']}"
                else:
                    url = run("gh", "pr", "create", "--repo", repo, "--base", "main", "--head", branch, "--title", f"chore: update {release} release pins", "--body-file", str(body), cwd=worktree)
                    activity = f"{release}: opened {url}"
            finally:
                body.unlink()
            return activity
        finally:
            run("git", "worktree", "remove", "--force", str(worktree), cwd=root)


def verify_orphan_branch(root: Path, branch: str, pins: list[ReleasePin]) -> None:
    base = run("git", "merge-base", "origin/main", "origin/" + branch, cwd=root)
    changed = set(run("git", "diff", "--name-only", base, "origin/" + branch, cwd=root).splitlines())
    if not changed or not changed.issubset({pin.path for pin in pins}):
        raise RuntimeError(f"existing branch needs manual review: {branch}")
    actor = os.environ["RELEASE_PIN_ACTOR"].removeprefix("app/") + "[bot]"
    commits = run("git", "rev-list", f"{base}..origin/{branch}", cwd=root).splitlines()
    if not commits:
        raise RuntimeError(f"existing branch has no updater commit: {branch}")
    for commit in commits:
        if run("git", "show", "-s", "--format=%an", commit, cwd=root) != actor:
            raise RuntimeError(f"existing branch has a non-updater commit: {branch}")
        if f"Signed-off-by: {actor} <" not in run("git", "show", "-s", "--format=%B", commit, cwd=root):
            raise RuntimeError(f"existing branch has an unsigned commit: {branch}")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, required=True, help="checkout containing the Helmfile releases")
    parser.add_argument("--repository", help="owner/name of the checkout to publish to")
    parser.add_argument("--publish", action="store_true", help="create or update issues and PRs")
    parser.add_argument("--limit", type=int, default=10, help="maximum releases processed per run")
    args = parser.parse_args()
    if args.limit < 1:
        parser.error("--limit must be positive")
    if args.publish and not args.repository:
        parser.error("--repository is required with --publish")
    root = args.root.resolve()
    registry = Registry(root)
    planned, skipped = plan(root, registry)
    summary = [f"## Daily release pin scan\n\n{len(planned)} releases have eligible updates.\n"]
    summary += [f"- `{release}`: " + ", ".join(f"{pin.kind} {pin.current} → {pin.target}" for pin in pins) for release, pins in sorted(planned.items())]
    summary += ["\n### Needs manual source mapping or review\n"] + [f"- {item}" for item in sorted(set(skipped))]
    failed = False
    if args.publish:
        activity, failed = publish(root, args.repository, planned, args.limit, registry.urls)
        summary += ["\n### PR activity\n"] + [f"- {item}" for item in activity]
    report = "\n".join(summary) + "\n"
    print(report)
    if destination := os.environ.get("GITHUB_STEP_SUMMARY"):
        with Path(destination).open("a") as file:
            file.write(report)
    if registry.checked == 0:
        print("release pin updater: no registries could be checked", file=sys.stderr)
        return 1
    if failed:
        print("release pin updater: one or more releases failed; see summary", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, RuntimeError, ValueError) as error:
        print(f"release pin updater: {error}", file=sys.stderr)
        raise SystemExit(1)
