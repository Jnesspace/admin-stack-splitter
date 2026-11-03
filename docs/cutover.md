# Manual cutover

Use the generated `CUTOVER.md` in your preparation directory for this procedure. It includes your source stack, worker pool, resource counts, child paths and mount table. Copy its setup block into your Bash shell to set `SPLIT_STACK_ID` and `SPLIT_PREPARED`. Keep those values for every step below.

Run every `admin-splitter` command locally, from the working directory containing your build. Spacelift runs the actual plans and applies on its workers. Keep the API credentials and endpoint from the quickstart available.

Before starting, check the state-file sizes in the generated mount table and arrange the private plan export described in step 6. The tool does not fetch run plans. Confirm that you can transfer each run's actual plan JSON to your local working directory before beginning cutover.

Complete one step at a time. Continue only when its check passes. If a command fails or a plan differs from the expected changes, keep the stacks frozen and resolve the failure before proceeding. The tool does not perform any of the account changes below.

## 1. Freeze the original admin

In Spacelift, prevent new automatic runs and applies on the original admin. Wait for executing runs to finish, then discard outstanding unapplied plans.

**Check:** no pending or executing run can change the original admin's state. Keep this freeze through the state and configuration changes.

## 2. Recheck the source snapshot

Fetch into a new directory, then compare the current state with the snapshot used for preparation:

```sh
./admin-splitter fetch --stack "$SPLIT_STACK_ID" --out ./source-recheck
./admin-splitter check-source \
  --state ./source-recheck/admin.tfstate \
  --prepared "$SPLIT_PREPARED/prepare.json"
```

If you use a separately exported snapshot, pass that fresh file to `check-source` instead. Use a different new destination if `source-recheck` already exists.

**Check:** `check-source` succeeds. It checks state lineage, serial and the exact snapshot checksum. Separately confirm that the applied configuration, worker pool, runner image and other inherited settings still match the prepared inputs, and that the freeze is still in place.

If any source input changed, prepare again from current matching inputs. Retain the original state backup and applied configuration revision.

## 3. Mount each child's state on the parent

Use the mount table at the top of the generated guide. For each row, create a write-only mounted file on the original parent stack:

- Upload the contents of the listed local state file.
- Use the exact relative mount path shown, such as `NAME.tfstate`.
- Set file mode to `600`.

That relative path becomes `/mnt/workspace/NAME.tfstate` on the runner. The parent reads these files when it creates the children and imports their states.

Spacelift limits mounted files to **2 MB**. Check each file's size in the generated table. For a larger snapshot, arrange an approved private delivery method that places it at the same `/mnt/workspace/NAME.tfstate` path with mode `600` before the parent plan and apply. Resolve that delivery before starting cutover. See [mounted-file limits and alternatives](https://docs.spacelift.io/concepts/configuration/environment#mounted-files).

**Check:** every table row has either a matching mount or a verified private delivery method. Keep state files out of Git, runner images and task logs.

## 4. Import the reduced parent state

Lock the original stack and ensure it has no pending runs or tasks. As a stack administrator, use **State history → Import state** and select the local file:

```text
$SPLIT_PREPARED/private/states/parent.tfstate
```

Resolve `$SPLIT_PREPARED` to the preparation directory from the setup block when using the file picker. Keep the original stack ID.

**Check:** the import succeeds on the original parent, and the imported state's resource count matches the parent count in the generated guide. Keep runs frozen.

The import requirements are documented in [Spacelift state management](https://docs.spacelift.io/vendors/terraform/state-management#importing-existing-state-file-into-your-terraform-stacks).

## 5. Publish the split configuration

Review and publish the contents of `$SPLIT_PREPARED/repo/` at the original admin's repository and project root. Include its `_split/` child directories. Publish only this configuration directory.

Keep runs paused while publishing. Discard stale runs queued during the change. Do not run liftoff generation or publication against the split tree.

**Check:** the next parent run will use the new configuration together with the reduced parent state. Confirm the published revision and child configuration paths against the generated guide.

## 6. Create and verify a fresh parent plan

Allow one fresh parent run using the new revision, with automatic apply off. Wait for its plan and obtain the plan JSON through the private export process arranged before cutover.

The JSON must come from that exact run's saved plan. For an OpenTofu run, the native worker-side export command is:

```sh
umask 077 && tofu show -json spacelift.plan > /mnt/workspace/parent-plan.json
```

Run this in the same run's `after_plan` phase through your approved export process, then transfer `parent-plan.json` privately to your local working directory. Spacelift documents access to `spacelift.plan` in [`after_plan`](https://docs.spacelift.io/concepts/policy/terraform-plan-policy#exposing-terraform-configuration-to-the-plan-policy). The command only writes a file; your export process must handle delivery. Do not print plan JSON in logs.

Back on your local machine, run:

```sh
./admin-splitter verify-plan \
  --plan ./parent-plan.json \
  --prepared "$SPLIT_PREPARED/prepare.json" \
  --allow-controller-create
```

If your export process supplies the binary plan instead, convert it locally with `tofu show -json parent.tfplan > parent-plan.json`, using the matching OpenTofu version and required providers. Always give [`tofu show`](https://opentofu.org/docs/cli/commands/show/) the saved plan filename: state JSON is not a valid plan.

**Check:** local verification succeeds. Existing managed resources have no changes; only the controller additions recorded by preparation are allowed. Review output changes separately.

## 7. Apply that parent plan

Manually confirm the same parent run whose plan passed step 6. If the run replans, verify the replacement plan before confirming it.

**Check:** the apply succeeds, creates the listed children and imports their states. Confirm the child stacks use the original worker pool shown in the generated guide.

## 8. Confirm dependency outputs

Check that a native tracked parent apply has published the dependency outputs needed by the children. A task-based apply or state import alone does not publish them.

If outputs are missing, perform a parent-only tracked run. Verify its managed-resource plan is unchanged before manually applying it.

**Check:** the required outputs are available to the children before starting their bootstrap runs.

## 9. Verify each child's bootstrap plan

For each child listed in the generated guide, manually start its first run with automatic apply off. Export its actual plan JSON using the same private process as for the parent. The native worker-side command for that child's run is:

```sh
umask 077 && tofu show -json spacelift.plan > /mnt/workspace/child-plan.json
```

Transfer the JSON privately, then verify it on your local machine:

```sh
./admin-splitter verify-plan --plan ./child-plan.json
```

Use the plan from the child currently under review. Keep each child's artifacts separate.

**Check:** every child plan has zero managed-resource changes and passes verification. Review any output changes. Stop on a create, update, delete or replacement of an existing managed resource.

## 10. Complete each child's bootstrap

Manually confirm each verified child run that requires an apply. If a plan changes, repeat its verification first.

**Check:** every child has completed its bootstrap successfully. Exactly one active state owns each original resource across the parent and children.

## 11. Remove temporary mounts and exports

After all imports and bootstrap runs are confirmed, manually remove the temporary child-state mounts. End any temporary private state delivery and plan-export setup, including export hooks you added. Preserve any configuration that existed before this procedure.

**Check:** no child import still depends on a mounted snapshot, and temporary exports are cleaned up. Keep the private backups for your recovery window.

## 12. Check conditional fan-out

Choose a reviewed change confined to one child's configuration. Manually run the parent, check its plan and apply it. Inspect which child runs are selected, then review each selected child's plan before allowing its apply.

**Check:** the intended child runs, unrelated children are not selected, and each actual plan matches the reviewed change. Investigate unexpected selection or resource changes before enabling automation.

## 13. Restore normal operation

Restore the intended triggers, automatic-apply settings and any temporary state-export setting according to your operating policy. Further edits belong in the child configuration paths shown in the generated guide.

**Check:** normal runs use the split configuration and expected worker pool.

Clear the local credentials when finished:

```sh
unset SPACELIFT_API_KEY_ID SPACELIFT_API_KEY_SECRET SPACELIFT_API_TOKEN
```

## If a step fails

Keep affected stacks frozen. Record which imports and applies succeeded, inspect the current parent and child states, and establish resource ownership before resuming.

Do not restore the full original parent state while active child states still own those resources. Do not force-push a backup over a backend that has received new changes. Recovery must coordinate matching state and configuration revisions; involve your Spacelift contact if ownership is uncertain.
