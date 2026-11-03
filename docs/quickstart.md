# Split your migration-kit admin stack

Build the tool from source, fetch your admin's settings and state directly from Spacelift, then prepare the split on your own workstation. You need Go and the matching OpenTofu version; no additional Spacelift CLI is required.

Preparation produces configuration and partitioned state files for review. Follow the [cutover guide](cutover.md) to publish them and transfer live ownership.

## 1. Build from source

Use Go 1.24 or later on macOS or Linux, and a Bash shell for the commands below. Clone the source repository and build the command:

```sh
git clone https://github.com/jnesspace/admin-stack-splitter.git
cd admin-stack-splitter
go build -o admin-splitter ./cmd/admin-splitter
./admin-splitter --help
```

If you already received a source checkout, run the build from its root. Keep the complete checkout, including `docs/cutover.md`, which is embedded during the build.

Create a private working directory outside the repository you will publish, then copy the executable you just built:

```sh
umask 077
mkdir -m 700 ../admin-split-work
cp ./admin-splitter ../admin-split-work/
cd ../admin-split-work
```

Run the remaining commands from this directory. Keep the source checkout for its documentation; the cutover guide is also included in each preparation as `CUTOVER.md`.

## 2. Configure access to your account

Use a [secret-based Spacelift API key](https://docs.spacelift.io/integrations/api#secret-based-api-keys) with permission to read the admin stack's settings and state. Replace these placeholders with your account URL and the stack ID from its URL:

```sh
export SPACELIFT_API_KEY_ENDPOINT='https://YOUR_ACCOUNT.app.spacelift.io'
SPLIT_STACK_ID='YOUR_ADMIN_STACK_ID'
```

Load `SPACELIFT_API_KEY_ID` and `SPACELIFT_API_KEY_SECRET` from your secret manager, or enter them at prompts in **Bash**:

```bash
set +x
unset SPACELIFT_API_TOKEN
read -r -p 'Spacelift API key ID: ' SPACELIFT_API_KEY_ID
read -r -s -p 'Spacelift API key secret: ' SPACELIFT_API_KEY_SECRET
printf '\n'
export SPACELIFT_API_KEY_ID SPACELIFT_API_KEY_SECRET
```

The tool exchanges the key for a session token itself. If you already have a session token, set `SPACELIFT_API_TOKEN` instead and leave both API-key variables unset. Do not put credentials in command arguments, source files or tickets.

State download requires Spacelift-managed state, **External state access** enabled on the admin stack, and the `Stack: Read stack state` and `Stack: Download stack state` permissions. The tool does not enable access for you. See [external state access](https://docs.spacelift.io/vendors/terraform/external-state-access).

## 3. Freeze the admin and fetch its inputs

Prevent new automatic runs on the original admin, wait for executing runs to finish, and discard unapplied plans. Keep this freeze in place from state capture through cutover.

Copy the exact applied migration-kit configuration into `./generated/`, including its complete module tree and provider lock file. Use the applied revision; do not rerun `liftoff generate` to obtain it.

Fetch the settings and current state:

```sh
./admin-splitter fetch --stack "$SPLIT_STACK_ID" --out ./source
```

The endpoint comes from `SPACELIFT_API_KEY_ENDPOINT`; you can also pass `--endpoint https://YOUR_ACCOUNT.app.spacelift.io`. The destination must be new. The tool writes `source/admin.tfstate` and `source/source-stack.json` with private permissions. It does not change stack configuration or live state.

Your working directory now contains:

```text
admin-split-work/
  admin-splitter
  generated/
    liftoff.tf
    ...space module directories and provider lock file...
  source/
    admin.tfstate
    source-stack.json
```

If external state access is unavailable, use the existing-snapshot procedure at the end of this guide. Running `tofu state pull` in this new local directory will not connect it to the admin's managed backend.

## 4. Match the OpenTofu version

```sh
./admin-splitter inspect --state ./source/admin.tfstate
```

Install the exact version shown in `tool_version` using the [OpenTofu installation instructions](https://opentofu.org/docs/intro/install/standalone/), then check `tofu version`. The splitter checks the version but does not download or upgrade OpenTofu. If the executable is outside PATH, pass `--tool /path/to/tofu` when preparing.

## 5. Preview and prepare

Preview the module boundaries:

```sh
./admin-splitter inspect --config ./generated --state ./source/admin.tfstate
```

Each populated leaf space module becomes a child. Shared ancestors remain in the original admin. Review the remaining parent count: a single large leaf or many root resources need a different split boundary. Optional explicit groups are described in [README.md](../README.md#choose-module-boundaries).

Prepare into a new directory:

```sh
./admin-splitter prepare \
  --config ./generated \
  --state ./source/admin.tfstate \
  --source-stack ./source/source-stack.json \
  --out ./prepared
```

Children inherit the original worker pool, runner image, repository, branch and supported administrator scope. Private workers stay private. The tool preserves provider requirements and lock files, partitions local state copies, and initializes and validates each new root. Provider downloads need network access; preparation with these saved inputs makes no account requests.

Automatic settings discovery supports standard OpenTofu admins using Spacelift-native or GitHub repositories, with one space-admin role binding on the admin's own space. Other VCS providers and permission scopes require a reviewed manual layout. Hooks, context attachments and cloud integrations are not cloned; review any custom runtime dependencies before cutover.

## 6. Review and cut over

```sh
cat prepared/prepare.json
```

Require `complete: true` and `validated: true`. Check the source stack ID, original worker pool, resource counts and child paths. Do not use `--validate=false` for a customer handoff.

| Output | Use |
| --- | --- |
| `prepared/repo/` | Parent and child configuration to review and publish |
| `prepared/private/original.tfstate` | Unchanged source backup |
| `prepared/private/states/` | Parent and child state partitions |
| `prepared/CUTOVER.md` | Live handoff and recovery instructions |

Publish only `prepared/repo/`. State files and saved plan JSON may contain secrets; keep the working directory private.

Follow `prepared/CUTOVER.md` once preparation is validated and the source is still frozen. If the source changes or you release the freeze, fetch fresh inputs and prepare into another new directory.

Keep credentials available for the state recheck at the start of cutover. Clear them when finished:

```sh
unset SPACELIFT_API_KEY_ID SPACELIFT_API_KEY_SECRET SPACELIFT_API_TOKEN
```

## Use existing snapshots instead of fetching

If you already have a current state export and matching stack settings, place them at `source/admin.tfstate` and `source/source-stack.json` and start at step 4. You do not need API credentials for this path.

Settings can be exported using [examples/source-stack.graphql](../examples/source-stack.graphql) from the source checkout, with `{"id":"YOUR_ADMIN_STACK_ID"}` as its variables. A Spacelift MCP response (`{"stack": ...}`) or GraphQL response (`{"data": {"stack": ...}}`) is accepted. Include all queried worker-pool, runner, VCS and role fields.

If state must be exported from an authenticated task, arrange its private transfer before that task's temporary workspace disappears. See [Spacelift state export](https://docs.spacelift.io/vendors/terraform/state-management#exporting-spacelift-managed-terraform-state-file). The configuration must match that state, and the same freeze and cutover checks still apply.
