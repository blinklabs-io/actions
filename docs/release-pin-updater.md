# Infrastructure release pin updater

The `Daily infrastructure release pins` workflow runs at 08:17 UTC each day in
this private repository. It checks static Helmfile chart versions and image tags
in `blinklabs-io/infrastructure`, then opens at most ten release-specific issues
and PRs per run when the repository variable
`INFRA_RELEASE_PINS_PUBLISH_ENABLED` is `true`. Until then, scheduled runs scan
without publishing. It never deploys or merges changes.

The workflow uses `GOVERNANCE_APP_CLIENT_ID` and `GOVERNANCE_APP_PRIVATE_KEY`,
which are already configured for central governance. The App installation must
include `infrastructure` and grant Contents, Issues, and Pull requests **read and
write** repository permissions. The workflow requests a token scoped only to
`infrastructure`. The App owner must approve any new permissions on the
installation before a publishing run can work.

A manual run defaults to a read-only scan. `auth_check_only` validates the
publication token and target access without scanning. `publish` runs the same
scan and publishes eligible changes. After the workflow reaches `main`, run the
auth-only and read-only modes, then set `INFRA_RELEASE_PINS_PUBLISH_ENABLED` to
`true` to enable scheduled publication.

An open updater PR is identified by its release marker and the governance App
author. A later run fetches that branch, merges current `main`, adds a signed-off
commit for newer pins, and updates its PR body. It never force-pushes or opens
another PR for that release while the first is open. A closed unmerged updater
PR is reported for manual review. Existing hand-maintained PRs and issues
mentioning the release also block duplicate updater PRs.

Cardano node updates must also initialize the candidate runtime with the selected
bundled network configuration. A failed check excludes the entire release,
including accompanying chart updates. Existing updater branches are checked
again after merging `main` and applying pins, before any push or PR-body update.
This preserves the current pins when a newer image cannot load its configuration.

For the Cardano node chart, static values files are applied in order to resolve
the image, `cardano_network`, and optional `cardano_config`. The chart's default
configuration path is `/opt/cardano/config/<network>/config.json`. Dynamic,
conditional, ambiguous, and non-bundled configurations require manual review.
The check uses the image's bundled topology; it does not validate a rendered
ConfigMap topology, chain synchronization, persistent data, or deployment readiness.

The candidate runs without network access, host mounts, or credentials. The
validator runs `cardano-node run` with closed stdin and `--shutdown-ipc 0`, using
the node's [graceful EOF shutdown handling](https://github.com/IntersectMBO/cardano-node/blob/11.1.3/cardano-node/src/Cardano/Node/Handlers/Shutdown.hs)
after [configuration and tracing initialization](https://github.com/IntersectMBO/cardano-node/blob/11.1.3/cardano-node/src/Cardano/Node/Run.hs).
Only exit status zero passes; startup errors, timeouts, and unavailable container
engines reject the update. Each process gets 30 seconds plus five seconds to
terminate; the outer container command is bounded to three minutes, including
image download. Linux uses Docker and macOS uses Apple Container.

The scanner reads files as text and does not decrypt Helmfile secrets. It
reports dynamic releases, shared values files, non-semver pins, direct
digest-pinned image references, and image references it cannot map to a
registry. Update source mappings in `cmd/release-pins/scan.go`
after checking the upstream chart's image default. Review generated PRs for
breaking changes and deployment ordering before merge.

For a local read-only scan, install Helm, `skopeo`, and the platform's container
engine, check out infrastructure,
then run from this repository:

```sh
go run ./cmd/release-pins --root /path/to/infrastructure
```
