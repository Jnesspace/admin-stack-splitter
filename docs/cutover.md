# Live cutover for your admin stack

Complete the customer quickstart (`docs/quickstart.md` in the source checkout) first. This procedure changes your live account and must be performed by an operator with the required stack, repository and state permissions. The preparation command does not execute these steps.

Paths below assume your shell is in the working directory containing your locally built `admin-splitter` and `prepared/`. Use the OpenTofu executable matching the snapshot version.

## 1. Confirm the freeze and source

Keep the original admin's automatic runs and applies paused. No run may change its state during the handoff. Finish executing runs and discard unapplied plans.

Check `prepared/prepare.json` is both complete and validated. Fetch the current source again into a new directory, using the same credentials, endpoint and stack ID from the quickstart:

```sh
./admin-splitter fetch --stack "$SPLIT_STACK_ID" --out ./source-recheck
./admin-splitter inspect --state ./source-recheck/admin.tfstate
```

Compare the new state's lineage, serial and SHA-256 with `source_lineage`, `source_serial` and `source_sha256` in the preparation report. On Linux use `sha256sum source-recheck/admin.tfstate`; on macOS use `shasum -a 256 source-recheck/admin.tfstate`. Confirm the original worker pool and other inherited settings are still current. If you used an existing snapshot instead of `fetch`, re-export it through the same approved method.

If the source changed, stop and prepare again from the matching configuration and fresh state. Retain `prepared/private/original.tfstate` and the original applied repository revision.

## 2. Review the new ownership and configuration

The existing stack remains the parent. Each group in `prepare.json` lists the child module address, state file and configuration path. Confirm the parent and child counts account for the original resources, and verify the intended worker pool and runner image.

Only `prepared/repo/` is publishable. Its contents replace the admin configuration at the original stack's repository/project-root location. The child project roots point into its `_split/` directory.

Do not run liftoff generation/publication against this split tree: it would restore the original single-admin wiring.

## 3. Mount the child snapshots temporarily

On the original parent stack, create a write-only mounted file for each group using the contents of:

```text
prepared/private/states/NAME.tfstate
```

Its relative mounted-file path must be `NAME.tfstate`, producing `/mnt/workspace/NAME.tfstate` on the runner. Set mode `600`. Use the actual group names from `prepare.json`.

These files are read when the parent creates each child and imports its state. Do not commit them to the repository or print them in task logs.

## 4. Switch parent state and configuration while frozen

Lock the original stack and ensure it has no pending runs or tasks. As a stack administrator, use the stack's **State history → Import state** action to import:

```text
prepared/private/states/parent.tfstate
```

Then publish the complete contents of `prepared/repo/` at the original admin project root. Keep runs paused throughout both operations. Discard any stale runs queued during the change; the next parent run must use the new configuration and reduced parent state together.

State import is documented in [Spacelift state management](https://docs.spacelift.io/vendors/terraform/state-management#importing-existing-state-file-into-your-terraform-stacks). If your account uses a different approved import workflow, use that workflow against the same original stack. Do not change the stack ID or recreate the parent.

## 5. Verify and apply the parent plan

After both changes are in place, permit a fresh parent run with automatic apply still off. Obtain its actual saved plan and convert it to JSON where the matching OpenTofu executable can read it:

```sh
tofu show -json parent.tfplan > parent-plan.json
./admin-splitter verify-plan \
  --plan parent-plan.json \
  --prepared prepared/prepare.json \
  --allow-controller-create
```

The filenames above refer to the saved plan from that run; `prepare` does not create a live plan. Arrange access to the run's plan artifact, or run the verifier on the runner with the preparation report and an executable built for that runner's operating system and architecture. The verifier is not automatically installed as a Spacelift hook.

For example, from the source checkout, build for a Linux x86-64 runner with:

```sh
GOOS=linux GOARCH=amd64 go build \
  -o ../admin-split-work/admin-splitter-linux-amd64 ./cmd/admin-splitter
```

Use the actual runner's operating system and architecture. Copy that executable to the runner as `admin-splitter` alongside the preparation report.

Existing managed resources must have no changes. Only the listed new controller resources may be created. Review output changes separately, then apply that same verified plan. Do not verify one plan and apply another.

The parent creates the child stacks, role bindings and dependencies, and imports each child's mounted state. The original managed resource IDs should remain unchanged.

## 6. Publish outputs and bootstrap children

Ensure a native tracked parent apply has published the dependency outputs. A task-based apply or state import does not publish them. If necessary, force a parent-only tracked apply after checking that its managed-resource plan is unchanged.

New children need an initial bootstrap run. Run every child once and check its actual saved plan:

```sh
tofu show -json child.tfplan > child-plan.json
./admin-splitter verify-plan --plan child-plan.json
```

Require zero managed-resource changes for every child. Review output-only changes and complete each required bootstrap apply. Keep automatic apply off until all children pass.

## 7. Check fan-out and restore normal operation

Check that exactly one active state owns each original resource. Test a reviewed change confined to one child's configuration and verify that the dependency flow selects the intended child. Investigate unexpected plans before enabling automation.

Remove the temporary child-state mounts once imports and bootstrap are confirmed. Restore triggers, apply settings and any temporary state-export settings according to your operating policy. Retain the private backups for your recovery window.

Further edits belong in the child configuration paths listed in `prepare.json`. Old generated subdirectories may remain as inactive copies.

## If a step fails

Keep the affected stacks frozen. Record which imports and applies succeeded, inspect the current parent and child states, and establish resource ownership before resuming.

Do not restore the full original parent state while active child states still own those resources. Do not force-push a backup over a backend that has received new changes. Recovery must coordinate the matching state and configuration revisions; involve your Spacelift contact if ownership is uncertain.
