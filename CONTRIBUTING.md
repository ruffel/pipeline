# Contributing

## Setup

Requires Go 1.26+, [just](https://github.com/casey/just), and
[golangci-lint](https://golangci-lint.run) v2.15.0, the version CI uses: older
versions reject the config.

The repo is a Go workspace: the core module at the root, `observers/terminal`
as a module of its own, and the examples together as a third.

## Workflow

```bash
just                 # test and lint the core and observers/terminal
just build-examples  # build the examples and run their tests
just check-clean     # fmt + tidy must leave no diff (enforced in CI)
just example 1       # run an example by its number
```

## Pull requests

- Keep PRs small and focused.
- Use conventional commit titles: `type(scope): Imperative subject`
  (e.g. `fix(core): Strip context cancellation during event emission`).
- Add tests for behaviour changes; event-sequence assertions preferred.
