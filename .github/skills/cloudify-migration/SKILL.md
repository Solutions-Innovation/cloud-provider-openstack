```skill
---
name: cloudify-migration
description: >
  Trigger V2O (VMware-to-OpenStack) migration workflows using the Cloudify CLI
  (`cfy`) for e2e testing of the iSCSI-Cinder CSI plugin (Phase 5 — CDI
  Multi-Phase Precopy). Covers warm and cold V2O migration blueprints on the
  conductor57 Cloudify manager. Use when the user wants to create a migration
  deployment, run install/cutover/uninstall workflows, monitor execution status,
  or clean up completed deployments.
license: Apache-2.0
---

# Cloudify V2O Migration — Trigger & Monitor

## When to activate

Activate this skill when the user says any of:
- "trigger migration", "run V2O", "start warm migration", "start cold migration"
- "cloudify deploy", "cfy deploy", "create migration deployment"
- "run cutover", "run install workflow", "check migration status"
- "list deployments", "list executions", "clean up deployment"
- Any reference to using Cloudify to test the iSCSI-Cinder CSI plugin e2e

Do NOT activate for:
- O2O (OpenStack-to-OpenStack) migrations — not yet ready, user will notify when available
- Building/deploying the CSI plugin itself — use `dev-deploy` skill instead
- Modifying CSI plugin code — use `nfs-cinder-csi` or code-level skills

## Environment setup

**CRITICAL:** Every terminal session MUST activate the Cloudify virtualenv first:
```bash
source ~/cloudifyenv/bin/activate
```

The Cloudify CLI is `cfy`. All `cfy` commands must run inside the activated virtualenv.
The `cfy` Python warnings about `pkg_resources` deprecation can be ignored.

## Cloudify profiles

Two Cloudify Manager profiles are configured:

| Profile | Manager IP | Default? |
|---------|-----------|---------|
| `conductor57.windriver.liquidweb` | conductor57.windriver.liquidweb | **Yes (active `*`)** |
| `conductor.windriver.liquidweb` | conductor.windriver.liquidweb | No |

**Default profile:** `conductor57.windriver.liquidweb`

To switch profiles:
```bash
# Use conductor57 (default for this skill)
cfy profiles use conductor57.windriver.liquidweb -u admin -p 'admin' --ssl --rest-certificate ~/manager57.crt

# Use conductor (alternate)
cfy profiles use conductor.windriver.liquidweb -u admin -p 'admin' --ssl --rest-certificate ~/manager.crt
```

To verify connectivity:
```bash
cfy deployments list
```

## Workflow

### Step 1: Choose a blueprint

**Before anything else**, list the uploaded blueprints on the active Cloudify manager:

```bash
source ~/cloudifyenv/bin/activate
cfy blueprints list 2>/dev/null
```

Parse the output and present a summary table to the user showing blueprint IDs,
descriptions, creation dates, and visibility.

Then use `ask_questions` to let the user pick:

**Question 1 — Blueprint** (single select):
- Header: `Blueprint`
- Question: `Which blueprint to use? (Found <N> blueprints on conductor57)`
- Options: **dynamically populated** from `cfy blueprints list` output. Show up to
  6 blueprints with format: `<blueprint_id>`. If there are more than 6, show the 6
  most recently uploaded and note the total count in the question text.
- allowFreeformInput: true — user can also type a blueprint ID directly

**Important:** The manager may have many blueprints beyond the well-known V2O ones.
Always list from the live CLI output — never hardcode the options. Blueprint IDs that
have been seen historically include `warm-migration`, `cold-migration`, `warm-rhoso`,
`cold-rhoso`, and various `o2o-*` blueprints, but the list changes over time.

### Step 2: List deployments for the chosen blueprint and choose action

After the user selects a blueprint, list existing deployments **filtered to that
blueprint**:

```bash
source ~/cloudifyenv/bin/activate
cfy deployments list 2>/dev/null | grep '<SELECTED_BLUEPRINT_ID>'
```

Also run the full `cfy deployments list` to get structured output. Filter rows where
the `blueprint_id` column matches the selected blueprint.

Present a summary table of matching deployments (deployment ID, display name, status,
created date).

Then use `ask_questions`:

**Question 1 — Deployment action** (single select):
- Header: `Action`
- Question: `Found <N> existing deployment(s) using blueprint "<BLUEPRINT_ID>". Reuse an existing deployment or create a new one?`
- Options:
  - `Reuse an existing deployment` — re-run workflows on an existing deployment
  - `Create a new deployment` (recommended) — create a fresh deployment with new inputs
  - `Update deployment inputs` — fix or change inputs on an existing deployment without recreating it
  - `Delete an existing deployment` — clean up a completed/failed deployment
- If **zero** deployments exist for this blueprint, skip this question and go
  directly to "Create a new deployment" (Step 3).

**If "Reuse an existing deployment":**

Use `ask_questions` to let the user pick from the filtered deployment list:

- Header: `Deployment`
- Question: `Which existing deployment? (blueprint: <BLUEPRINT_ID>)`
- Options: dynamically populated from the filtered deployments list. Show up to
  6 most recent deployments with format: `<display_name> (<deployment_id>)`
- allowFreeformInput: true — user can also paste a deployment ID directly

After selection, **skip to Step 7** and follow the exact same workflow as a newly
created deployment: list node instances (Step 7), kick off execution (Step 8), and
repeat for additional workflows (Step 9). The reuse path and the create-new path
converge at Step 7 — there is no difference in how workflows are executed.

**If "Update deployment inputs":**

Use this when a deployment was created with incorrect input values (e.g.,
`enable_boot_from_volume: "false"` instead of `"true"`) and you want to fix
them without deleting and recreating the deployment.

1. Ask which deployment to update (same dynamic options as above).

2. Retrieve current inputs:
   ```bash
   source ~/cloudifyenv/bin/activate
   cfy deployments inputs <DEPLOYMENT_ID> 2>/dev/null
   ```

3. Ask the user which input(s) to change. Present the current values so the user
   can confirm what needs fixing.

4. Write a corrected inputs YAML file to `/tmp/migration-inputs-fix.yaml` that
   contains **all** deployment inputs (not just the changed ones). Copy the
   existing values and apply only the requested changes.

5. Run the update with all lifecycle operations skipped:
   ```bash
   source ~/cloudifyenv/bin/activate
   cfy deployments update <DEPLOYMENT_ID> \
     -b <BLUEPRINT_ID> \
     -i /tmp/migration-inputs-fix.yaml \
     --skip-install --skip-uninstall --skip-reinstall
   ```

   The `--skip-install --skip-uninstall --skip-reinstall` flags ensure that only
   the inputs are updated — no node lifecycle operations are triggered.

6. Verify the update succeeded:
   ```bash
   cfy deployments inputs <DEPLOYMENT_ID> 2>/dev/null
   ```

7. After verification, **skip to Step 7** (list node instances and choose workflow).

**Note:** The `-b <BLUEPRINT_ID>` flag is **required** by `cfy deployments update`.
  Use the same blueprint the deployment was originally created with. You can find it
  from the `cfy deployments list` output.

**If "Delete an existing deployment":**

Ask which deployment to delete (same dynamic options as above), then run:
```bash
cfy executions start uninstall -d <DEPLOYMENT_ID> --timeout 900
cfy deployments delete <DEPLOYMENT_ID>
```

**If "Create a new deployment":** Continue to Step 3.

### Step 3: Gather new deployment parameters (interactive)

The blueprint was already selected in Step 1. Now use `ask_questions` to collect the
remaining migration configuration. Batch into max 3 questions.

**Fast-path for iSCSI CSI e2e testing:** If the user answers with "default",
"go ahead", or selects all recommended/default options, skip remaining questions
and use the pre-built e2e defaults from `assets/e2e-iscsi-defaults.yaml`.

```bash
cp .github/skills/cloudify-migration/assets/e2e-iscsi-defaults.yaml /tmp/migration-inputs.yaml
```
Then skip to Step 6 (create deployment).

**Otherwise, proceed with interactive questions:**

**Question 1 — Source VM name** (freeform):
- Header: `Source VM`
- Question: `VMware source VM name to migrate?`
- Options:
  - `fedora-bios` (recommended) — known test VM
  - `warm-test`
- allowFreeformInput: true

**Question 2 — Storage class** (freeform):
- Header: `StorageClass`
- Question: `Kubernetes StorageClass for migration PVC?`
- Options:
  - `general` (recommended) — default NFS-backed StorageClass
  - `csi-sc-cinder-iscsi` — iSCSI-Cinder CSI StorageClass (for iSCSI e2e testing)
- allowFreeformInput: true

**Question 3 — Deployment ID** (freeform):
- Header: `Deploy ID`
- Question: `Deployment ID? (Leave empty to auto-generate as <src_vm_name>-<blueprint_id>)`
- allowFreeformInput: true

### Step 4: Confirm and set profile

Ensure the correct profile is active:
```bash
source ~/cloudifyenv/bin/activate
cfy profiles use conductor57.windriver.liquidweb -u admin -p 'admin' --ssl --rest-certificate ~/manager57.crt
```

Verify connectivity:
```bash
cfy deployments list 2>/dev/null | tail -5
```

If this fails, report the error and **stop**.

### Step 5: Create the inputs YAML file

Write a temporary inputs file based on the selected blueprint and user responses.
Use the **warm** template if the blueprint ID contains `warm`. Use the **cold**
template if it contains `cold`. For any other/unknown blueprint, inspect
`cfy blueprints get <BLUEPRINT_ID>` to determine the required inputs.

See [references/input-templates.md](references/input-templates.md) for the full
warm and cold YAML templates with all fields.

Replace `<SRC_VM_NAME>` with the user's source VM name and `<STORAGE_CLASS>` with
the user's chosen StorageClass.

### Step 6: Create the deployment (do NOT execute)

```bash
source ~/cloudifyenv/bin/activate
cfy deployments create <DEPLOYMENT_ID> \
  -b <BLUEPRINT_ID> \
  -i /tmp/migration-inputs.yaml
```

Wait for the `create_deployment_environment` execution to complete:
```bash
cfy executions list -d <DEPLOYMENT_ID>
```

If deployment creation fails, report the error and **stop**.

**Important:** Do NOT automatically start the `install` workflow. The deployment is
created but not executed. Continue to Step 7 to let the user choose which workflow
and which nodes to execute.

### Step 7: Choose workflow and select node instances

After the deployment is created (or when reusing an existing deployment), list the
available node instances:

```bash
source ~/cloudifyenv/bin/activate
cfy node-instances list -d <DEPLOYMENT_ID> 2>/dev/null
```

Parse the output and present a summary table showing: node instance ID, node ID,
state, and host ID.

Then use `ask_questions` with up to 2 questions:

**Question 1 — Workflow** (single select):
- Header: `Workflow`
- Question: `Deployment "<DEPLOYMENT_ID>" has <N> node instances. Which workflow to execute?`
- Options:
  - `install` (recommended) — start migration (precopy for warm, full copy for cold)
  - `cutover` — final sync + VM creation (warm migration only)
  - `uninstall` — cleanup migration resources
  - `Check status only` — just list executions, don't run anything

**If "Check status only":** Run `cfy executions list -d <DEPLOYMENT_ID>`, report
results, and **stop**.

**Question 2 — Node selection** (multi-select):
- Header: `Nodes`
- Question: `Which nodes to include? (Default: all nodes in the deployment)`
- Options: **dynamically populated** from the `cfy node-instances list` output.
  Extract the unique **node IDs** (not node instance IDs) from the listing.
  **Present nodes in the known execution order** (see "Known node orderings" below).
  If the blueprint is not in the known orderings list, **ask the user** what the
  correct execution order is before presenting options.
  Show each node ID as an option. Mark all options as selected by default
  (recommend all).
  Show up to 6 node IDs. If there are more than 6, show the first 6 and
  note the total. The user can type additional node IDs via freeform.
- multiSelect: true
- allowFreeformInput: true — user can type node IDs directly

**Node ID vs Node Instance ID:** Node filtering uses the `-p` (parameters) flag
with `node_ids` as a JSON list. The node ID is the blueprint-level node name,
not the node instance ID (which has a suffix like `_abcdef`).

#### Known node orderings

For known blueprints, present and pass `node_ids` in the following order.
**Order matters** — it determines the execution sequence on the Cloudify manager.

**warm-migration / warm-rhoso (install):**
1. `precheck-vm-migration`
2. `cleanup-env`
3. `fullcopy-source-vm`
4. `precopy-source-vm`
5. `cutover-w-lastcpy`
6. `create-guest-vm`

**cold-migration / cold-rhoso (install):**
1. `precheck-vm-migration`
2. `cleanup-env`
3. `fullcopy-source-vm`
4. `create-guest-vm`

**Unknown blueprints:** If the blueprint is not listed above, **ask the user** for
the correct node execution order before proceeding. Store new orderings in this
section for future reference.

### Step 8: Prepare install-params.yaml and kick off execution

Based on the user's node selection from Step 7, write a YAML parameters file
with the selected nodes **in the correct known order**, then pass it via `-p`.

**Always write the params file** — even when all nodes are selected. This ensures
the node ordering is explicit and correct.

1. Take the user's selected nodes from Step 7.
2. Sort them according to the **known node orderings** above (preserving only
   the selected nodes, in blueprint order).
3. Write `/tmp/install-params.yaml`:

```bash
cat > /tmp/install-params.yaml << 'EOF'
node_ids:
  - <NODE_ID_1>
  - <NODE_ID_2>
  - ...
EOF
```

4. Execute the workflow:
```bash
source ~/cloudifyenv/bin/activate
cfy executions start <WORKFLOW> -d <DEPLOYMENT_ID> \
  -p /tmp/install-params.yaml \
  --timeout 3600
```

**Note:** This CLI version does NOT support `--node-id` flags. Node filtering
is done exclusively via the `-p` workflow parameter with a YAML file.

**Important:** The `--timeout` flag controls how long the CLI waits for output, NOT
the execution timeout. The execution continues on the server even if the CLI times out.
Use `--timeout 3600` (1 hour) for install/cutover and `--timeout 900` for uninstall.

### Step 8b: Auto-uninstall on precheck failure

If the `install` workflow **fails** and the failure occurred on the
`precheck-vm-migration` node (the first node in the execution order), the source
VM is not qualified for migration. Common causes include:
- Source VM has existing snapshots that must be deleted
- Source VM is powered off (must be powered on)
- Changed Block Tracking (CBT) is not enabled on the source VM
- Source VM has unsupported hardware or configuration

**When this happens, automatically run uninstall** to clean up the failed
execution state — do NOT ask the user first. Then notify the user and wait:

1. Detect the failure: check `cfy executions list -d <DEPLOYMENT_ID>` — the
   latest `install` execution shows `failed` status.

2. Immediately run uninstall to reset node instances:
   ```bash
   source ~/cloudifyenv/bin/activate
   cfy executions start uninstall -d <DEPLOYMENT_ID> --timeout 900
   ```

3. Report the failure to the user with:
   - The specific error from the failed install execution
   - That the uninstall has been run automatically to clean up
   - A list of common source VM fixes (delete snapshots, power on, enable CBT)

4. **Stop and wait** for the user to confirm the source VM is fixed before
   re-running the install workflow. When the user confirms, go back to
   **Step 7** (list node instances, choose workflow, select nodes).

**This auto-uninstall only applies to `precheck-vm-migration` failures.** If a
later node fails (e.g., `fullcopy-source-vm`, `precopy-source-vm`), do NOT
auto-uninstall — report the error and ask the user how to proceed, as partial
migration state may need investigation.

### Step 9: Run additional workflows (cutover, uninstall, etc.)

After the current execution completes, return to the **Step 7** pattern:
- List node instances again (state may have changed)
- Ask which workflow to run next
- Ask which nodes to include
- Execute

**Typical warm migration sequence:**
1. `install` (all nodes or subset) → precopy starts
2. Wait for precopy period
3. `cutover` (all nodes or subset) → final sync + VM creation
4. `uninstall` (all nodes) → cleanup

**Typical cold migration sequence:**
1. `install` (all nodes or subset) → full disk copy + VM creation
2. `uninstall` (all nodes) → cleanup

**For each subsequent workflow**, repeat the `ask_questions` prompts from Step 7 so
the user always has the choice of which nodes to include. Do NOT assume the same node
selection carries over between workflows.

### Step 10: Verify migration result

After the final workflow completes:
```bash
cfy executions list -d <DEPLOYMENT_ID>
```

All executions should show `completed` status. Report each execution's workflow
name, status, duration, and total execution time.

For CDI validation at each phase, read
[references/cdi-lifecycle.md](references/cdi-lifecycle.md) — it contains expected
resource states tables for every workflow step (fullcopy, precopy, cutover, etc.).

### Step 11: Cleanup

After testing is complete, clean up the deployment:

```bash
cfy executions start uninstall -d <DEPLOYMENT_ID> --timeout 900
cfy deployments delete <DEPLOYMENT_ID>
```

## References

This skill uses **progressive disclosure** per the Agent Skills spec. The main
SKILL.md contains the core workflow. Detailed reference material is in separate
files — read them on demand when deeper context is needed.

| File | Content | When to read |
|------|---------|-------------|
| [references/cdi-lifecycle.md](references/cdi-lifecycle.md) | CDI Volume Populator architecture, checkpoint flow, code paths, expected resource states by phase, StorageProfile prerequisite | Debugging CDI state, validating e2e test progress, understanding PVC Prime pattern |
| [references/input-templates.md](references/input-templates.md) | Full warm/cold YAML templates, iSCSI CSI e2e context, input defaults tables | Creating inputs files (Step 5), understanding CSI lifecycle during migration |
| [references/monitoring.md](references/monitoring.md) | Cloudify CLI monitoring commands, K8s CDI monitoring commands, debugging common failures, edge cases | Monitoring migration progress, troubleshooting failures |
| [assets/e2e-iscsi-defaults.yaml](assets/e2e-iscsi-defaults.yaml) | Pre-built inputs for iSCSI CSI e2e fast-path | Fast-path deployment creation (Step 3) |

## Rules

- **ALWAYS activate the virtualenv** (`source ~/cloudifyenv/bin/activate`) before any `cfy` command
- **ALWAYS use conductor57 profile** unless the user explicitly requests conductor
- **ALWAYS list blueprints first** (`cfy blueprints list`) and ask which blueprint to use — the manager may have many blueprints beyond the well-known V2O ones
- **ALWAYS list deployments filtered to the chosen blueprint** before asking whether to reuse or create new — there are typically many deployments on the manager
- **ALWAYS ask for `src_vm_name` and `storage_class`** when creating a new deployment — never assume
- **ALWAYS show the deployment ID and execution status** after each workflow step
- **NEVER automatically execute a workflow after creating a deployment** — always list node instances first and let the user choose the workflow and which nodes to include
- **ALWAYS list node instances** (`cfy node-instances list -d <DEPLOYMENT_ID>`) before each workflow execution so the user can select which nodes to include
- **ALWAYS present node selection as multi-select** with all nodes selected by default — the user can deselect nodes they want to skip
- **NEVER embed passwords in inputs files** — use Cloudify secret references (e.g., `vcenter_password`, `openstack_password` are secret refs, not actual passwords)
- **NEVER run cutover without confirming** the user is ready (warm migration only)
- **ALWAYS pass node_ids in the correct execution order** — order matters and determines the sequence of operations on the Cloudify manager. Use the known orderings in the "Known node orderings" section.
- **ALWAYS ask the user for node ordering** when encountering an unknown blueprint not listed in the known orderings section — never guess the order
- **ALWAYS use `-p node_ids=` for node filtering** — this CLI version does NOT support `--node-id` flags
- If any step fails, **report the error and stop** — do not silently continue
- **AUTO-UNINSTALL on precheck failure** — if `install` fails on `precheck-vm-migration`, automatically run `uninstall` to clean up, report the error, and wait for the user to fix the source VM before retrying (see Step 8b)
- Clean up `/tmp/migration-inputs.yaml` after deployment creation succeeds
```
