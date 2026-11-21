"""Behavior checks for release discovery and safe pin edits."""

import hashlib
import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import release_pin_updater as updater


class FakeRegistry:
    def digest(self, reference: str) -> str:
        return "sha256:test"

    def versions(self, source: str) -> list[str]:
        return {
            "oci://ghcr.io/example/charts/service": ["1.2.3", "1.2.4", "2.0.0-rc1"],
            "ghcr.io/example/service": ["1.2.3", "1.2.4"],
        }[source]

    def verify(self, pin: updater.ReleasePin) -> None:
        if pin.target != "1.2.4":
            raise AssertionError("unexpected unpublished version")


class ReleasePinUpdaterTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        (self.root / "helmfile-app/service").mkdir(parents=True)
        (self.root / updater.HELMFILE).write_text(
            "releases:\n"
            "  - name: service\n"
            "    chart: oci://ghcr.io/example/charts/service\n"
            "    version: 1.2.3\n"
            "    values:\n"
            "      - service/values.yaml\n"
        )
        (self.root / "helmfile-app/service/values.yaml").write_text(
            'image:\n  repository: ghcr.io/example/service\n  tag: "1.2.3"\n'
        )

    def test_cli_scans_explicit_target_checkout_and_repository(self) -> None:
        pin = updater.ReleasePin("service", "chart", str(updater.HELMFILE), 0,
                                 "1.2.3", "1.2.4", "oci://ghcr.io/example/charts/service")
        with patch.object(sys, "argv", ["release_pin_updater.py", "--root", str(self.root),
                                        "--repository", "example/repo", "--publish"]), \
             patch.object(updater, "Registry") as registry, \
             patch.object(updater, "plan", return_value=({"service": [pin]}, [])) as plan, \
             patch.object(updater, "publish", return_value=([], False)) as publish:
            registry.return_value.checked = 1
            registry.return_value.urls = {}
            self.assertEqual(updater.main(), 0)
        plan.assert_called_once_with(self.root.resolve(), registry.return_value)
        publish.assert_called_once_with(self.root.resolve(), "example/repo", {"service": [pin]}, 10, {})

    def test_registry_tags_and_manifest_digest(self) -> None:
        (self.root / updater.REPOS).write_text("repositories: []\n")
        registry = updater.Registry(self.root)
        with patch.object(updater, "run", return_value='{"Tags": ["1.2.3"]}') as run:
            self.assertEqual(registry.versions("oci://ghcr.io/example/chart"), ["1.2.3"])
            run.assert_called_once_with("skopeo", "list-tags", "docker://ghcr.io/example/chart")
        manifest = b'{"schemaVersion":2,"manifests":[]}\n'
        with patch.object(updater.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, manifest)):
            self.assertEqual(registry.digest("ghcr.io/example/image:1.2.3"),
                             "sha256:" + hashlib.sha256(manifest).hexdigest())
        with patch.object(updater.subprocess, "run", side_effect=subprocess.CalledProcessError(1, "skopeo")):
            with self.assertRaises(subprocess.CalledProcessError):
                registry.digest("ghcr.io/example/missing:1.2.3")

    def test_plan_and_edit_chart_and_image_for_one_release(self) -> None:
        planned, skipped = updater.plan(self.root, FakeRegistry())
        self.assertEqual(skipped, [])
        self.assertEqual([(pin.kind, pin.target) for pin in planned["service"]],
                         [("chart", "1.2.4"), ("image", "1.2.4")])
        updater.apply_pins(self.root, "service", planned["service"])
        self.assertIn("version: 1.2.4", (self.root / updater.HELMFILE).read_text())
        self.assertIn('tag: "1.2.4"', (self.root / "helmfile-app/service/values.yaml").read_text())

    def test_shared_values_are_reported_and_not_edited(self) -> None:
        with (self.root / updater.HELMFILE).open("a") as file:
            file.write(
                "  - name: second\n"
                "    chart: charts/local\n"
                "    values:\n"
                "      - service/values.yaml\n"
            )
        planned, skipped = updater.plan(self.root, FakeRegistry())
        self.assertEqual([pin.kind for pin in planned["service"]], ["chart"])
        self.assertEqual(sum("shared values file" in item for item in skipped), 2)

    def write_ranged_dingonodes(self) -> None:
        (self.root / updater.HELMFILE).write_text(
            "releases:\n"
            '{{- range $network := list "preview" "preprod" }}\n'
            "  - name: dingonodes-{{ $network }}\n"
            "    chart: oci://ghcr.io/example/charts/dingonodes\n"
            "    version: 0.1.0\n"
            "    labels:\n"
            "      network: {{ $network }}\n"
            "    values:\n"
            "      - dingonodes/defaults.yaml.gotmpl\n"
            "      - dingonodes/{{ $network }}/values.{{ $network }}.yaml.gotmpl\n"
            "{{- end }}\n"
            "\n"
            "  - name: {{ .Values.dynamic }}\n"
            "    chart: oci://ghcr.io/example/charts/other\n"
        )
        base = self.root / "helmfile-app/dingonodes"
        (base / "defaults.yaml.gotmpl").parent.mkdir(parents=True)
        (base / "defaults.yaml.gotmpl").write_text("nodes: {}\n")
        for network in ("preview", "preprod"):
            (base / network).mkdir()
            (base / network / f"values.{network}.yaml.gotmpl").write_text(
                "nodes:\n"
                f"  {network}-a:\n"
                "    spec:\n"
                "      image:\n"
                "        repository: ghcr.io/example/dingo\n"
                '        tag: "0.94.2"\n'
                '        digest: "sha256:old"\n'
            )

    def test_ranged_release_block_yields_one_spec_per_network(self) -> None:
        self.write_ranged_dingonodes()
        specs, skipped = updater.releases(self.root)
        self.assertEqual([spec.name for spec in specs], ["dingonodes-preview", "dingonodes-preprod"])
        self.assertEqual(specs[1].values, (
            "dingonodes/defaults.yaml.gotmpl",
            "dingonodes/preprod/values.preprod.yaml.gotmpl",
        ))
        self.assertEqual(specs[1].version, "0.1.0")
        self.assertEqual(len(skipped), 1)
        self.assertIn("dynamic release name or chart", skipped[0])

    def test_ranged_release_block_ends_at_its_matching_end(self) -> None:
        (self.root / updater.HELMFILE).write_text(
            "releases:\n"
            "{{- if .Values.enabled }}\n"
            '{{- range $network := list "a" "b" }}\n'
            "  - name: first-{{ $network }}\n"
            "    chart: oci://ghcr.io/example/charts/first\n"
            "    {{- if .Values.pinned }}\n"
            "    version: 0.1.0\n"
            "    {{- end }}\n"
            "    values:\n"
            "      - first/{{ $network }}.yaml\n"
            "  - name: second-{{ $network }}\n"
            "    chart: oci://ghcr.io/example/charts/second\n"
            "    version: 0.2.0\n"
            "    {{- with .Values.extra }}{{ . }}{{- end }}\n"
            "{{- end }}\n"
            "{{- end }}\n"
            "  - name: plain\n"
            "    chart: oci://ghcr.io/example/charts/plain\n"
            "    version: 1.0.0\n"
        )
        specs, skipped = updater.releases(self.root)
        self.assertEqual(skipped, [])
        self.assertEqual([(spec.name, spec.values) for spec in specs], [
            ("first-a", ("first/a.yaml",)),
            ("first-b", ("first/b.yaml",)),
            ("second-a", ()),
            ("second-b", ()),
            ("plain", ()),
        ])
        self.assertEqual([spec.chart_owner for spec in specs], ["", "first-a", "", "second-a", ""])

    def test_values_range_release_stays_skipped(self) -> None:
        (self.root / updater.HELMFILE).write_text(
            "releases:\n"
            "  {{- range .Values.instances }}\n"
            "  - name: instance-{{ .name }}\n"
            "    chart: oci://ghcr.io/example/charts/instance\n"
            "    version: 0.1.0\n"
            "  {{- end }}\n"
            '{{- range $network := list "a" }}\n'
            "  - name: node-{{ $network }}\n"
            "    chart: oci://ghcr.io/example/charts/node\n"
            "    version: 0.2.0\n"
            "{{- end }}\n"
        )
        specs, skipped = updater.releases(self.root)
        self.assertEqual([spec.name for spec in specs], ["node-a"])
        self.assertEqual(skipped, ["instance-{{ .name }}: dynamic release name or chart"])

    def test_ranged_release_image_pin_is_planned_and_applied_per_network(self) -> None:
        self.write_ranged_dingonodes()

        class Registry(FakeRegistry):
            def versions(self, source: str) -> list[str]:
                return {
                    "oci://ghcr.io/example/charts/dingonodes": ["0.1.0", "0.1.1"],
                    "ghcr.io/example/dingo": ["0.94.2", "0.95.0"],
                }[source]

            def verify(self, pin: updater.ReleasePin) -> None:
                return None

        planned, skipped = updater.plan(self.root, Registry())
        self.assertEqual(sorted(planned), ["dingonodes-preprod", "dingonodes-preview"])
        self.assertEqual([(pin.kind, pin.target) for pin in planned["dingonodes-preview"]],
                         [("chart", "0.1.1"), ("image", "0.95.0")])
        self.assertEqual([(pin.kind, pin.target) for pin in planned["dingonodes-preprod"]],
                         [("image", "0.95.0")])
        self.assertIn("dingonodes-preprod: chart version shared with dingonodes-preview", skipped)
        self.assertEqual(sum("shared values file" in item for item in skipped), 2)
        updater.apply_pins(self.root, "dingonodes-preprod", planned["dingonodes-preprod"])
        preprod = (self.root / "helmfile-app/dingonodes/preprod/values.preprod.yaml.gotmpl").read_text()
        self.assertIn('tag: "0.95.0"', preprod)
        self.assertIn('digest: "sha256:test"', preprod)
        preview = (self.root / "helmfile-app/dingonodes/preview/values.preview.yaml.gotmpl").read_text()
        self.assertIn('tag: "0.94.2"', preview)
        self.assertIn('digest: "sha256:old"', preview)
        self.assertIn("version: 0.1.0", (self.root / updater.HELMFILE).read_text())
        updater.apply_pins(self.root, "dingonodes-preview", planned["dingonodes-preview"])
        self.assertIn("version: 0.1.1", (self.root / updater.HELMFILE).read_text())

    def test_local_chart_digest_image_is_reported(self) -> None:
        (self.root / updater.HELMFILE).write_text(
            "releases:\n  - name: local\n    chart: charts/local\n"
        )
        chart = self.root / "helmfile-app/charts/local"
        chart.mkdir(parents=True)
        (chart / "values.yaml").write_text("image: python:3.13-slim@sha256:abc\n")
        planned, skipped = updater.plan(self.root, FakeRegistry())
        self.assertEqual(planned, {})
        self.assertTrue(any("direct image reference" in item for item in skipped))

    def test_dynamic_and_prerelease_versions_are_not_promoted(self) -> None:
        self.assertIsNone(updater.newest("1.2.3", ["2.0.0-rc1", "latest", "v2.0.0"]))
        self.assertIsNone(updater.newest("{{ .Values.tag }}", ["1.2.4"]))

    def test_digest_moves_with_tag(self) -> None:
        path = self.root / "helmfile-app/service/values.yaml"
        path.write_text(path.read_text() + '  digest: "sha256:old"\n')
        pin = updater.ReleasePin("service", "image", "helmfile-app/service/values.yaml",
                                 0, "1.2.3", "1.2.4", "ghcr.io/example/service", "sha256:new")
        updater.apply_pins(self.root, "service", [pin])
        self.assertIn('digest: "sha256:new"', path.read_text())

    def test_existing_pr_gets_new_signed_commit_without_rewrite(self) -> None:
        bare = self.root / "remote.git"
        updater.run("git", "init", "--bare", str(bare))
        updater.run("git", "init", "-b", "main", cwd=self.root)
        updater.run("git", "config", "user.name", "Test Bot", cwd=self.root)
        updater.run("git", "config", "user.email", "test@example.com", cwd=self.root)
        updater.run("git", "remote", "add", "origin", str(bare), cwd=self.root)
        updater.run("git", "add", "helmfile-app", cwd=self.root)
        updater.run("git", "commit", "-s", "-m", "test: initial pins", cwd=self.root)
        updater.run("git", "push", "origin", "main", cwd=self.root)
        self.addCleanup(lambda: updater.run("git", "worktree", "prune", cwd=self.root))
        original_run = updater.run
        calls: list[str] = []

        def fake_run(*args: str, cwd: Path | None = None) -> str:
            if args[:2] == ("gh", "pr"):
                calls.append(args[2])
                return "https://github.com/example/repo/pull/1"
            return original_run(*args, cwd=cwd)

        first = updater.ReleasePin("service", "chart", str(updater.HELMFILE), 0,
                                   "1.2.3", "1.2.4", "oci://ghcr.io/example/charts/service")
        issue: updater.IssueReference = {"number": 7}
        with patch.object(updater, "run", fake_run):
            result = updater.publish_one(self.root, "example/repo", "service", [first], {}, None, issue)
            assert result is not None
            self.assertIn("opened", result)
            branch = "chore/update-service-issue-7"
            previous = original_run("git", "rev-parse", "origin/" + branch, cwd=self.root)
            existing: updater.PublishedPR = {"headRefName": branch, "number": 1,
                        "body": updater.description("service", [first], {}, 7),
                        "url": "https://github.com/example/repo/pull/1"}
            second = updater.ReleasePin("service", "chart", str(updater.HELMFILE), 0,
                                        "1.2.3", "1.2.5", first.source)
            result = updater.publish_one(self.root, "example/repo", "service", [second], {}, existing, issue)
            assert result is not None
            self.assertIn("updated", result)
            current = original_run("git", "rev-parse", "origin/" + branch, cwd=self.root)
            self.assertNotEqual(previous, current)
            self.assertEqual(original_run("git", "rev-parse", current + "^", cwd=self.root), previous)
            self.assertIn("Signed-off-by: Test Bot <test@example.com>",
                          original_run("git", "show", "-s", "--format=%B", current, cwd=self.root))
            result = updater.publish_one(self.root, "example/repo", "service", [second], {}, existing, issue)
            assert result is not None
            self.assertIn("refreshed", result)
            existing["body"] = updater.description("service", [second], {}, 7)
            self.assertIsNone(updater.publish_one(self.root, "example/repo", "service", [second], {}, existing, issue))
            (self.root / "README.md").write_text("main advanced\n")
            original_run("git", "add", "README.md", cwd=self.root)
            original_run("git", "commit", "-s", "-m", "test: advance main", cwd=self.root)
            original_run("git", "push", "origin", "main", cwd=self.root)
            self.assertIn("updated", updater.publish_one(self.root, "example/repo", "service", [second], {}, existing, issue) or "")
            merged = original_run("git", "rev-parse", "origin/" + branch, cwd=self.root)
            self.assertNotEqual(merged, current)
            self.assertEqual(original_run("git", "merge-base", "origin/main", merged, cwd=self.root),
                             original_run("git", "rev-parse", "origin/main", cwd=self.root))
            self.assertIn("Signed-off-by: Test Bot <test@example.com>",
                          original_run("git", "show", "-s", "--format=%B", merged, cwd=self.root))
        self.assertEqual(calls, ["create", "edit", "edit"])

    def test_pushed_branch_is_reused_after_pr_creation_fails(self) -> None:
        bare = self.root / "remote.git"
        updater.run("git", "init", "--bare", str(bare))
        updater.run("git", "init", "-b", "main", cwd=self.root)
        updater.run("git", "config", "user.name", "release-bot[bot]", cwd=self.root)
        updater.run("git", "config", "user.email", "123+release-bot[bot]@users.noreply.github.com", cwd=self.root)
        updater.run("git", "remote", "add", "origin", str(bare), cwd=self.root)
        updater.run("git", "add", "helmfile-app", cwd=self.root)
        updater.run("git", "commit", "-s", "-m", "test: initial pins", cwd=self.root)
        updater.run("git", "push", "origin", "main", cwd=self.root)
        self.addCleanup(lambda: updater.run("git", "worktree", "prune", cwd=self.root))
        original_run = updater.run
        create_attempts = 0

        def fake_run(*args: str, cwd: Path | None = None) -> str:
            nonlocal create_attempts
            if args[:3] == ("gh", "pr", "create"):
                create_attempts += 1
                if create_attempts == 1:
                    raise RuntimeError("temporary PR API failure")
                return "https://github.com/example/repo/pull/2"
            return original_run(*args, cwd=cwd)

        pin = updater.ReleasePin("service", "chart", str(updater.HELMFILE), 0,
                                 "1.2.3", "1.2.4", "oci://ghcr.io/example/charts/service")
        issue: updater.IssueReference = {"number": 7}
        with patch.dict(os.environ, {"RELEASE_PIN_ACTOR": "app/release-bot"}), \
             patch.object(updater, "run", fake_run):
            with self.assertRaisesRegex(RuntimeError, "temporary PR API failure"):
                updater.publish_one(self.root, "example/repo", "service", [pin], {}, None, issue)
            self.assertIn("refs/heads/chore/update-service-issue-7",
                          original_run("git", "ls-remote", "--heads", "origin", cwd=self.root))
            result = updater.publish_one(self.root, "example/repo", "service", [pin], {}, None, issue)
        self.assertEqual(result, "service: opened https://github.com/example/repo/pull/2")
        self.assertEqual(create_attempts, 2)

    def test_untrusted_issue_marker_cannot_be_reused(self) -> None:
        pin = updater.ReleasePin("service", "chart", str(updater.HELMFILE), 0,
                                 "1.2.3", "1.2.4", "oci://ghcr.io/example/charts/service")
        issue = {"number": 7, "title": "service release pins", "body": updater.description("service", [pin], {}),
                 "author": {"login": "another-user"}}

        def fake_run(*args: str, cwd: Path | None = None) -> str:
            if args[:3] == ("gh", "pr", "list"):
                return "[]"
            if args[:3] == ("gh", "issue", "list"):
                return json.dumps([issue])
            raise AssertionError(f"unexpected write: {args}")

        with patch.dict(os.environ, {"RELEASE_PIN_ACTOR": "app/release-bot"}), \
             patch.object(updater, "run", fake_run), \
             patch.object(updater, "publish_one") as publish_one:
            activity, failed = updater.publish(self.root, "example/repo", {"service": [pin]}, 10, {})
        self.assertTrue(failed)
        self.assertIn("untrusted", activity[0])
        publish_one.assert_not_called()

    def test_other_pr_title_or_issue_body_blocks_duplicate(self) -> None:
        pin = updater.ReleasePin("service", "chart", str(updater.HELMFILE), 0,
                                 "1.2.3", "1.2.4", "oci://ghcr.io/example/charts/service")
        pr = {"number": 1, "title": "Upgrade service", "body": "", "headRefName": "manual",
              "baseRefName": "main", "isCrossRepository": False, "author": {"login": "maintainer"},
              "url": "https://github.com/example/repo/pull/1"}
        issue = {"number": 2, "title": "Upgrade chart", "body": "The service release needs an update.",
                 "author": {"login": "maintainer"}}

        def fake_run(*args: str, cwd: Path | None = None) -> str:
            if args[:3] == ("gh", "pr", "list"):
                return json.dumps([pr] if "open" in args else [])
            if args[:3] == ("gh", "issue", "list"):
                return json.dumps([issue])
            raise AssertionError(f"unexpected write: {args}")

        with patch.dict(os.environ, {"RELEASE_PIN_ACTOR": "app/release-bot"}), \
             patch.object(updater, "run", fake_run), \
             patch.object(updater, "publish_one") as publish_one:
            activity, failed = updater.publish(self.root, "example/repo", {"service": [pin]}, 10, {})
            self.assertFalse(failed)
            self.assertIn("another open PR", activity[0])
            pr["title"] = "Unrelated"
            activity, failed = updater.publish(self.root, "example/repo", {"service": [pin]}, 10, {})
        self.assertFalse(failed)
        self.assertIn("another open issue", activity[0])
        publish_one.assert_not_called()

    def test_branch_chart_source_change_rejects_stale_plan(self) -> None:
        pin = updater.ReleasePin("service", "chart", str(updater.HELMFILE), 0,
                                 "1.2.3", "1.2.4", "oci://ghcr.io/example/charts/service")
        path = self.root / updater.HELMFILE
        path.write_text(path.read_text().replace("charts/service", "charts/other"))
        with self.assertRaisesRegex(RuntimeError, "chart source changed"):
            updater.apply_pins(self.root, "service", [pin])

    def test_untrusted_marker_is_never_published(self) -> None:
        pin = updater.ReleasePin("service", "chart", str(updater.HELMFILE), 0,
                                 "1.2.3", "1.2.4", "oci://ghcr.io/example/charts/service")
        pr: updater.OpenPR = {"number": 1, "title": "service release pins", "body": updater.description("service", [pin], {}, 7),
              "headRefName": "chore/update-service-issue-7", "baseRefName": "main",
              "isCrossRepository": True, "author": {"login": "attacker"},
              "url": "https://github.com/example/repo/pull/1"}

        def fake_run(*args: str, cwd: Path | None = None) -> str:
            if args[:3] == ("gh", "pr", "list"):
                return json.dumps([pr] if "open" in args else [])
            if args[:3] == ("gh", "issue", "list"):
                return "[]"
            raise AssertionError(f"unexpected write: {args}")

        with patch.dict(os.environ, {"RELEASE_PIN_ACTOR": "app/release-bot"}), \
             patch.object(updater, "run", fake_run), \
             patch.object(updater, "publish_one") as publish_one:
            activity, failed = updater.publish(self.root, "example/repo", {"service": [pin]}, 10, {})
        self.assertTrue(failed)
        self.assertIn("untrusted", activity[0])
        publish_one.assert_not_called()

    def test_existing_app_pr_author_is_trusted_and_wrong_author_is_rejected(self) -> None:
        pin = updater.ReleasePin("service", "chart", str(updater.HELMFILE), 0,
                                 "1.2.3", "1.2.4", "oci://ghcr.io/example/charts/service")
        pr: updater.OpenPR = {"number": 1, "title": "service release pins", "body": updater.description("service", [pin], {}, 7),
              "headRefName": "chore/update-service-issue-7", "baseRefName": "main",
              "isCrossRepository": False, "author": {"login": "release-maintainer"},
              "url": "https://github.com/example/repo/pull/1"}

        def fake_run(*args: str, cwd: Path | None = None) -> str:
            if args[:3] == ("gh", "pr", "list"):
                return json.dumps([pr] if "open" in args else [])
            if args[:3] == ("gh", "issue", "list"):
                return "[]"
            raise AssertionError(f"unexpected call: {args}")

        with patch.dict(os.environ, {"RELEASE_PIN_ACTOR": "app/release-bot"}), \
             patch.object(updater, "run", fake_run), \
             patch.object(updater, "publish_one", return_value="service: updated") as publish_one:
            pr["author"]["login"] = "app/release-bot"
            activity, failed = updater.publish(self.root, "example/repo", {"service": [pin]}, 10, {})
            self.assertFalse(failed)
            self.assertEqual(activity, ["service: updated"])
            self.assertEqual(publish_one.call_args.args[5], pr)

            pr["author"]["login"] = "other-maintainer"
            publish_one.reset_mock()
            activity, failed = updater.publish(self.root, "example/repo", {"service": [pin]}, 10, {})
            self.assertTrue(failed)
            self.assertIn("untrusted", activity[0])
            publish_one.assert_not_called()


if __name__ == "__main__":
    unittest.main()
