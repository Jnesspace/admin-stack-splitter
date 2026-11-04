# Working on admin-stack-splitter

Use Go 1.27.3 or later. Run commands from the repository root.

## Build

```sh
go build -o admin-splitter ./cmd/admin-splitter
./admin-splitter --help
```

`cmd/admin-splitter` is the entry point; implementation and tests live in `internal/splitter`. Customer documentation lives in `docs/`. The build embeds `docs/cutover.md`, so keep it with the source checkout.

## Test

Install OpenTofu and ensure `tofu` is on PATH before running the tests. State-partition tests invoke the real executable and skip when it is absent; a passing test run without OpenTofu does not validate state movement.

```sh
go test -race ./...
go vet ./...
```

To also initialize providers and validate the generated configuration roots:

```sh
ADMIN_SPLITTER_VALIDATE_INTEGRATION=1 go test \
  -run 'TestCustomerFetchInspectPrepareWorkflow|TestPrepare.*NativeValidation|TestPublishNativeValidation' ./...
```

These checks need network access to provider registries. They use generic fixtures and local API test servers, with no live Spacelift credentials. They do not prove a live customer cutover or a zero-change plan against an account.

## Changes

Keep customer identities, credentials, state snapshots and compiled executables out of Git. Use generic fixtures. Update the affected customer guide when commands or behavior change, and preserve the checks that every original resource has exactly one state owner after preparation.

Keep human output useful for an operator and preserve per-command `--json` for scripts. The generated cutover guide adds split-specific details to the single canonical procedure in `docs/cutover.md`; do not duplicate that procedure in Go and keep applies manual. `publish --confirm` is the explicit write boundary. Its HTTPS test server checks upload credentials, operation ordering, refusal paths and recovery records; native integration tests validate the import-based controller.
