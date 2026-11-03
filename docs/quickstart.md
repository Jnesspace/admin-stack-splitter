# Prepare your admin stack split

Run every `admin-splitter` command on your local machine. This guide builds the tool, captures your admin's inputs and prepares a split for review. Spacelift runs the actual plans and applies; each live change is a separate manual step in the generated cutover guide. Start with Go 1.24 or later on macOS or Linux and a Bash shell. No additional Spacelift CLI is required.

Complete each step before continuing. If a command fails, resolve its error first.

## 1. Build and create a working directory

```sh
git clone https://github.com/jnesspace/admin-stack-splitter.git
cd admin-stack-splitter
go build -o admin-splitter ./cmd/admin-splitter
./admin-splitter --help
```

If you already have the source checkout, start with the build command from its root. Keep the complete checkout, including `docs/cutover.md`, which the build embeds.

Create a private directory outside the repository you will publish:

```sh
umask 077
mkdir -m 700 ../admin-split-work
cp ./admin-splitter ../admin-split-work/
cd ../admin-split-work
```

**Check:** help lists the available commands. Run the remaining commands from this working directory.

## 2. Set your account and credentials

Use a [secret-based Spacelift API key](https://docs.spacelift.io/integrations/api#secret-based-api-keys) with permission to read the admin's settings and state. Replace the account URL and stack ID below:

```sh
export SPACELIFT_API_KEY_ENDPOINT='https://YOUR_ACCOUNT.app.spacelift.io'
SPLIT_STACK_ID='YOUR_ADMIN_STACK_ID'
```

Load the key variables from your secret manager, or enter them at these Bash prompts:

```bash
set +x
unset SPACELIFT_API_TOKEN
read -r -p 'Spacelift API key ID: ' SPACELIFT_API_KEY_ID
read -r -s -p 'Spacelift API key secret: ' SPACELIFT_API_KEY_SECRET
printf '\n'
export SPACELIFT_API_KEY_ID SPACELIFT_API_KEY_SECRET
```

An existing `SPACELIFT_API_TOKEN` can be used instead; leave both API-key variables unset in that case.

**Check:** the account URL and stack ID identify your original admin. State download requires managed state, **External state access**, and the `Stack: Read stack state` and `Stack: Download stack state` permissions. The tool does not enable access. See [external state access](https://docs.spacelift.io/vendors/terraform/external-state-access).

## 3. Freeze the admin and copy its applied configuration

In Spacelift, prevent new automatic runs and applies on the original admin. Wait for executing runs to finish and discard outstanding unapplied plans. Keep this freeze through cutover.

Copy the exact applied migration-kit configuration into `./generated/`, preserving the complete module tree and provider lock file. Use the applied repository revision; do not rerun `liftoff generate`.

**Check:** `generated/liftoff.tf` and its space module directories are present, and the original admin cannot change state.

## 4. Fetch the settings and state

```sh
./admin-splitter fetch --stack "$SPLIT_STACK_ID" --out ./source
```

The endpoint comes from the environment; `--endpoint URL` can override it. The destination must be new.

**Check:** the summary confirms that `source/admin.tfstate` and `source/source-stack.json` were saved. These files stay private. If external access is unavailable, use the existing-snapshot procedure below.

## 5. Match the OpenTofu version

```sh
./admin-splitter inspect --state ./source/admin.tfstate
```

Install the exact OpenTofu version shown by `inspect` using the [installation instructions](https://opentofu.org/docs/intro/install/standalone/), then check `tofu version`. The splitter does not install or upgrade it.

**Check:** the versions match. If the executable is outside PATH, add `--tool /path/to/tofu` to preparation.

## 6. Review the proposed groups

```sh
./admin-splitter inspect --config ./generated --state ./source/admin.tfstate
```

Each populated leaf space becomes a child. Shared ancestors stay in the parent.

**Check:** the groups and remaining parent count make sense for your estate. A single large leaf or many root resources may need [explicit module boundaries](../README.md#choose-module-boundaries).

## 7. Prepare the split

```sh
./admin-splitter prepare \
  --config ./generated \
  --state ./source/admin.tfstate \
  --source-stack ./source/source-stack.json \
  --out ./prepared
```

Preparation preserves the original worker pool and supported runtime settings, partitions local state copies, and validates the new roots. Provider downloads need network access. It makes no live account changes.

**Check:** the summary reports that preparation and validation succeeded. Confirm the original worker pool, parent count and child counts. Do not skip validation for cutover.

## 8. Open your generated cutover guide

Open `prepared/CUTOVER.md`. Its first section contains your stack ID, original worker pool, counts, child paths and the exact state-file mount table.

Copy the setup block at the top of that guide into your shell. Then follow its manual steps in order: freeze and recheck, mount child states, import parent state, publish configuration, verify and apply the parent plan, bootstrap each child, and restore operation.

**Check:** the source is still frozen and you understand the [supported inputs](../README.md#supported-input). Source hooks, context attachments and cloud integrations are not cloned; review custom runtime dependencies before proceeding.

Only `prepared/repo/` is publishable. Keep `source/`, `prepared/private/`, plan artifacts and credentials out of Git. If the source changes or the freeze is released, fetch fresh inputs and prepare again into a new directory.

## Use existing snapshots instead of fetching

If you already have a current state export and matching stack settings, place them at `source/admin.tfstate` and `source/source-stack.json`, then start at step 5. No API credentials are needed for local preparation.

Export settings with [examples/source-stack.graphql](../examples/source-stack.graphql), using `{"id":"YOUR_ADMIN_STACK_ID"}` as its variables. MCP's `{"stack": ...}` and GraphQL's `{"data": {"stack": ...}}` responses are accepted; include all queried fields.

If state is exported from an authenticated task, transfer it privately before the temporary workspace disappears. See [Spacelift state export](https://docs.spacelift.io/vendors/terraform/state-management#exporting-spacelift-managed-terraform-state-file). A fresh local directory is not connected to the admin's backend. Matching configuration, the freeze and a fresh source recheck remain required.
