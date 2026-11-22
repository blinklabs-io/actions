# Agent instructions

## Private repository configuration

This repository is a private governance controller. Private repositories may be
declared directly in `repos-config.yaml`, including their names and the settings
needed to manage them. Use `private: true` on the repository entry when creating
a private repository, and use a shared profile for common configuration.

Verify the controller is still private before adding confidential entries. Keep
private repository names and configuration out of public repositories, issues,
pull requests, generated files, and workflow inputs or outputs. Do not commit
credentials. Preserve the existing redaction of private details in drift reports.

## Comments

Comments explain an invariant, a non-obvious algorithm, or a gotcha: why this
order, why this bound, why the obvious thing is wrong. Delete comments that
restate the code below them, label sections, or narrate a change's history.
Prose explaining how a system works belongs in documentation.

Doc comments on exported identifiers are the exception. They are published API
documentation: keep them accurate and in `// Name ...` form.
