# Contributing

## Setup

Requires Go 1.25+, [just](https://github.com/casey/just), and
[golangci-lint](https://golangci-lint.run) (version pinned in CI).

The repo is a Go workspace: the core module at the root, `observers/terminal`
as a submodule, and the examples as standalone modules.

## Workflow

```bash
just            # test + lint everything
just check-clean # fmt + tidy must leave no diff (enforced in CI)
just demo       # run the demo pipeline
```

## Pull requests

- Keep PRs small and focused.
- Use conventional commit titles: `type(scope): Imperative subject`
  (e.g. `fix(core): Strip context cancellation during event emission`).
- Add tests for behaviour changes; event-sequence assertions preferred.
