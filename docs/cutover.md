# Publish through the API

Use the setup block in your generated `CUTOVER.md` to set `SPLIT_STACK_ID` and `SPLIT_PREPARED`. Keep the exact applied source configuration and private backups. Run these commands locally with the same OpenTofu version used for preparation.

## 1. Upload the prepared split

Finish or discard pending source runs and tasks. The source can be disabled, but must be unlocked so the command can acquire its own lock. Use a user session or personal API key with state-import permissions; machine credentials cannot import managed state.

```sh
./admin-splitter publish --prepared "$SPLIT_PREPARED"
./admin-splitter publish --prepared "$SPLIT_PREPARED" --confirm
```

The command rechecks source state and settings, pauses and locks the parent, creates a new Spacelift-managed repository, creates disabled children with their states already loaded, imports the reduced parent state, and points the existing parent at the new repository. It enables the parent, starts a proposed run and releases its lock. Automatic apply is off. No plan is applied.

**Check:** the command prints a proposed-run link. All child stacks remain disabled. The parent retains its original ID and worker pool. No child-state mounts are needed; uploads support states up to 100 MiB. Original files under `prepared/repo/` remain unchanged; the actual published configuration is saved privately under `private/publish/published-repo/`.

## 2. Review and apply the parent

Open that proposed run in Spacelift and wait for planning to finish. The expected plan is:

- One import for each child `spacelift_stack.admin_split_NAME` at the ID recorded in `private/publish/review.json`.
- One new `spacelift_policy.admin_split_push`.
- For each child, one new role attachment, policy attachment, stack dependency and dependency reference.
- **Zero changes to existing resources**, including no updates, deletes or replacements.

That is **N imports and 1 + 4 × N creates** for N children. Review output changes too. The proposed plan cannot apply. Start a tracked run of the same published commit in Spacelift, review that plan again, and confirm only when it has the same expected changes. The command sets the parent's current commit to this revision so a normal run uses the published configuration.

For optional machine checking of privately exported saved-plan JSON:

```sh
./admin-splitter verify-plan --plan ./parent-plan.json \
  --prepared "$SPLIT_PREPARED/private/publish/review.json" \
  --allow-controller-create
```

This command requires the actual saved plan and checks its OpenTofu version, exact imports and exact creates. It rejects unexpected existing-resource changes. The tool does not download plan JSON or export it through run logs.

**Check:** the parent apply succeeds, adopts the child stacks and creates the wiring. Its `split_NAME_payload` outputs are available. Keep children disabled until this check passes.

## 3. Bootstrap the children

Enable one child in Spacelift and start a run of the published commit, with automatic apply off. Review its plan: it must have **zero managed-resource changes**. Confirm any required output-only apply. Repeat for each child. If privately exported JSON is available:

```sh
./admin-splitter verify-plan --plan ./child-plan.json \
  --prepared "$SPLIT_PREPARED/prepare.json"
```

**Check:** all children have the original pool, their imported resources and the parent dependency inputs. No existing resource was recreated. State import alone does not publish dependency outputs; the preceding parent apply is required.

## 4. Check future edits

In the new Spacelift repository, make a reviewed change confined to one `_split/NAME/` directory. Review and apply the parent run, then inspect the child runs selected by the changed dependency payload.

**Check:** only the intended child is selected; unrelated children are skipped. Review that child's actual plan before applying. Continue editing the split repository. Restore any desired automation only after these checks pass.

## If API publication fails

The command records each pending/completed step in `private/publish/recovery.json`, with original parent settings and backups beside it. It attempts to disable the parent again and reports whether that pause succeeded. Children stay disabled. There is no automatic rollback or resume: a timed-out write may have succeeded remotely, so a second publication is refused.

1. Verify in Spacelift that the parent and all recorded children are disabled. Finish or discard any pending runs. Keep the recovery directory private.
2. Compare the journal with current repository, child stacks and parent state. A `pending` step has an uncertain outcome; inspect it before retrying anything.
3. If restoring the original layout **before any parent or child apply**, remove only this publication's disabled child stacks without destroying their managed resources, restore the original parent state and original VCS/runtime settings together, then review a zero-change parent plan before restoring operation. Keep the new repository until recovery is verified.
4. If any apply has occurred, compare current states and resource ownership before choosing a recovery path. Do not overwrite newer state with a backup or activate two owners of the same resources.

The original state is `private/publish/original.tfstate`; original settings and completed IDs are in `recovery.json`. Use Spacelift's state-import UI/API while the parent is locked and idle. Resolve ownership uncertainty with your Spacelift contact before enabling runs.

---

# Manual alternative

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

**Check:** local verification succeeds. The plan uses the OpenTofu version recorded by preparation, existing managed resources have no changes, and it creates exactly the controller additions recorded by preparation. Review output changes separately.

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
./admin-splitter verify-plan \
  --plan ./child-plan.json \
  --prepared "$SPLIT_PREPARED/prepare.json"
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
