# Working in this repository

This is a source-only utility for splitting an existing migration-kit / liftoff admin stack. Customers build it with Go. Keep it small and independent of the migration-kit repository.

## Layout

- `cmd/admin-splitter/`: thin entry point and exit status.
- `internal/splitter/`: command handling, API reads, configuration rendering, state partitioning and plan checks. Tests live beside the implementation; generic fixtures live in `testdata/`.
- `docs/`: customer instructions. `docs/cutover.md` is embedded after a generated section with the split's actual IDs, paths and mount table. Keep one canonical copy of the manual procedure.
- `examples/`: generic layout and GraphQL examples.

## Behavior to preserve

- Every `admin-splitter` command runs locally. Actual plans and applies run in Spacelift; plan JSON is exported privately for local verification. Do not require installing the utility on workers.
- `inspect` previews grouping. `prepare` writes local copies and validates configuration; neither changes live infrastructure or remote state. Do not add automatic cutover or apply behavior without an explicit request.
- `fetch` may authenticate and request a temporary download URL. It reads settings and state without changing stack settings, worker capacity or remote state.
- Commands print human-readable summaries by default and accept per-command `--json` for automation. Keep both outputs accurate and exclude secrets.
- `check-source` compares state lineage, serial and checksum against the preparation report. It does not verify the live freeze, configuration or runtime settings; those remain manual checks.
- Preserve each original resource address, ID and attributes with exactly one owner across the resulting states. Keep the original parent lineage and distinct child lineages. Use native OpenTofu state operations on local copies.
- Inherit the original worker pool and supported runtime settings. Missing worker-pool metadata must fail; never silently fall back to public workers.
- Match the state writer's OpenTofu version and retain provider requirements and lock files. Do not install or upgrade tools implicitly.
- Keep the source snapshot unchanged. Outputs must use new private directories, protect snapshots from Git, and refuse overwrites.
- `verify-plan` must require actual plan JSON and reject existing-resource changes, deletes and replacements. Parent creation exceptions must match a validated preparation report exactly.
- Local validation does not prove a live zero-change plan or successful cutover. Describe test evidence accurately.
- Keep live cutover steps separate and manual, with an expected result before continuing. Commands in the canonical guide use the generated `SPLIT_STACK_ID` and `SPLIT_PREPARED` values without replacing them with defaults.

## Privacy and scope

Use generic names and synthetic values in code, fixtures, docs and commit messages. Do not commit customer identities, account URLs, real stack/run/pool IDs, credentials, state, saved plans, logs or machine-specific paths. The public project URL is allowed.

Read API credentials from the environment. Keep tokens, signed URLs, response bodies and state values out of output and errors. Require HTTPS, bounded responses and timeouts; do not follow redirects or forward account credentials to state downloads.

Do not add prebuilt binaries, release packaging or live benchmark infrastructure to this repository. Keep necessary tests. Do not edit sibling repositories as part of this tool's changes. Commit and push only when requested; do not add assistant attribution.

## Checks

From the repository root, with OpenTofu on PATH:

```sh
go build -o /tmp/admin-splitter ./cmd/admin-splitter
go test -race ./...
go vet ./...
ADMIN_SPLITTER_VALIDATE_INTEGRATION=1 go test \
  -run 'TestCustomerFetchInspectPrepareWorkflow|TestPrepare.*NativeValidation' ./...
```

State tests skip without OpenTofu; provider initialization also needs registry access. Tests use synthetic state and local API servers. Run `gofmt` on changed Go files and update affected customer instructions when behavior changes. See `CONTRIBUTING.md` for details.
