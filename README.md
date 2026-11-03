# Admin stack splitter

Prepare a **migration-kit / liftoff admin stack** for splitting into a parent and smaller child admins. The tool reads generated configuration, a matching state snapshot, and the existing admin stack's settings. It partitions local copies while preserving resource addresses and attributes.

Build from source with Go 1.24 or later:

```sh
git clone https://github.com/jnesspace/admin-stack-splitter.git
cd admin-stack-splitter
go build -o admin-splitter ./cmd/admin-splitter
./admin-splitter --help
```

Start with the [quickstart](docs/quickstart.md) for API credentials, state capture, version matching and preparation on your own workstation. The tool authenticates directly with Spacelift; no additional Spacelift CLI is needed.

Read the [cutover guide](docs/cutover.md) before scheduling the change. Publishing configuration and moving live backends remains an operator procedure. The build embeds this guide and writes it into each preparation as `CUTOVER.md`.

## Inherit the existing stack

Set `SPACELIFT_API_KEY_ID`, `SPACELIFT_API_KEY_SECRET` and `SPACELIFT_API_KEY_ENDPOINT` as described in the quickstart, then fetch your source stack:

```sh
./admin-splitter fetch --stack YOUR_ADMIN_STACK_ID --out ./source
```

This saves the current state and stack settings in a new private directory. An existing session token in `SPACELIFT_API_TOKEN` can be used instead of the API-key pair. The fetch command issues authentication and state-download requests; it does not change stack configuration or live state.

With the applied configuration in `./generated/` and the matching OpenTofu version installed, prepare from these saved inputs:

```sh
./admin-splitter prepare \
  --config ./generated \
  --state ./source/admin.tfstate \
  --source-stack ./source/source-stack.json \
  --out ./prepared
```

Children inherit the original worker pool, runner image, repository, branch, VCS integration, project root, space and same-space administrator role. **A private source pool stays private.** Missing pool metadata is an error; an explicitly public source stays public. Preparation with saved inputs makes no account requests; initialization may download providers.

If you already have a state snapshot, `prepare --stack YOUR_ADMIN_STACK_ID` can read the settings directly using the same credentials and endpoint. For account access through MCP, export [examples/source-stack.graphql](examples/source-stack.graphql) and supply it with `--source-stack`. MCP's `{"stack": ...}` and GraphQL's `{"data": {"stack": ...}}` envelopes are accepted.

The OpenTofu version comes from the state. Install that exact version or pass `--tool /path/to/tofu`. Existing provider requirements and lock files are preserved. The tool does not install or upgrade OpenTofu.

## Choose module boundaries

```sh
./admin-splitter inspect --config ./generated --state ./source/admin.tfstate
```

Each populated leaf space becomes a child by default. Shared ancestors stay in the parent. Review the remaining parent count: a single huge leaf or many root resources need a different split boundary.

For explicit groups, add `--layout choices.json` alongside the source-stack options:

```json
{
  "groups": [
    {"name": "team_a", "module": "module.organization.module.team_a"},
    {"name": "team_b", "module": "module.organization.module.team_b"}
  ]
}
```

Only `groups` and root input `variables` are accepted as choices. Runtime settings come from the source. Group names must be unique; `parent` is reserved. For manual integration work, [examples/layout.json](examples/layout.json) is a full layout and requires an explicit worker pool. Manual layouts do not discover source settings.

## Outputs and safeguards

```text
prepared/
  .gitignore                   # excludes private snapshots and logs
  prepare.json                 # source identity, counts, pool, validation
  CUTOVER.md                   # ordered live handoff and recovery
  repo/                        # publishable parent configuration
    _split/NAME/               # authoritative child configuration roots
  private/
    original.tfstate           # unchanged source backup
    states/parent.tfstate      # original lineage, selected resources removed
    states/NAME.tfstate         # distinct lineages, original addresses
```

The destination must be new and outside the source configuration. Directories use mode 700; files use mode 600. Source configuration and state remain unchanged. Publish **only `prepared/repo/`**.

Native `tofu state mv` operates on local copies without backend configuration. Checks account for each original instance exactly once and compare its address and attributes. Preparation runs `init -backend=false` and `validate` on every root. Require `complete: true` and `validated: true`; `--validate=false` is diagnostic and cannot authorize controller creation.

After cutover, edit child paths listed in `prepare.json`. Original subdirectories may remain as inactive copies. Do not run liftoff generation/publication over the split tree: it recreates the original single-admin wiring.

## Plan checks and cutover

```sh
tofu show -json parent.tfplan > parent-plan.json
./admin-splitter verify-plan --plan parent-plan.json \
  --prepared prepared/prepare.json --allow-controller-create

tofu show -json child.tfplan > child-plan.json
./admin-splitter verify-plan --plan child-plan.json
```

Only recorded controller addresses may be created. Existing changes, deletes and replacements fail. Child bootstrap plans require zero managed-resource changes. State JSON is rejected. Review output changes separately.

Follow the [cutover guide](docs/cutover.md) to freeze runs, recheck the source snapshot, coordinate configuration/state replacement and temporarily mount child snapshots for import. A native tracked parent apply must publish dependency outputs after a task-based apply or state import. Bootstrap every child before testing conditional fan-out. No remote state writes or applies are performed by this utility.

## Supported input

- Current kit output: unindexed nested space modules, inherited context IDs, and `concat` / `merge` wiring.
- Standard OpenTofu admins. Settings discovery supports native repositories and default/custom GitHub integrations, with the kit's same-space space-admin role. Other VCS providers or custom permission scopes need a reviewed manual layout.
- Empty root provider configurations. Aliases, configured providers, explicit provider mappings, cross-module outputs, module dependencies and external assets require manual adaptation.
- Local dependencies must stay inside each selected subtree. Escaping sibling references, cycles, symlinks and hidden dependency directories fail before partitioning.
- Sensitive boundary inputs are refused. Only Terraform files and provider lock files are copied; backend/cloud blocks are removed.

Source hooks, context attachments and cloud integrations are not cloned. Review customized runtime dependencies before handoff.

## Validation scope

Tests use generic configuration derived from migration-kit output and synthetic state. They verify partition ownership, IDs, attributes, shared inputs, source runtime settings and native configuration validation. They do not prove a customer-account zero-change plan or remote cutover.

## Source layout

- `cmd/admin-splitter/` starts the CLI.
- `internal/splitter/` contains command handling, API access, preparation and plan checks, with tests and generic fixtures.
- `docs/` contains the customer guides and embeds the cutover procedure.
- `examples/` contains a manual layout and a stack-settings query.

See [CONTRIBUTING.md](CONTRIBUTING.md) for build and test commands.

## Dependencies

HCL edits configuration syntax; cty evaluates expressions and renders typed values. License names were checked in dependency sources.

| Dependency | License |
| --- | --- |
| github.com/hashicorp/hcl/v2 | MPL-2.0 |
| github.com/zclconf/go-cty | MIT |
| github.com/agext/levenshtein | Apache-2.0 |
| github.com/apparentlymart/go-textseg/v15 | MIT |
| github.com/google/go-cmp | BSD-3-Clause |
| github.com/mitchellh/go-wordwrap | BSD-3-Clause |
| golang.org/x/mod | BSD-3-Clause |
| golang.org/x/sync | BSD-3-Clause |
| golang.org/x/text | BSD-3-Clause |
| golang.org/x/tools | BSD-3-Clause |
