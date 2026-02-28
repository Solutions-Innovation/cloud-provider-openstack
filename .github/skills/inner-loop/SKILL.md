````skill
---
name: inner-loop
description: >
  Enforces the inner development loop: after any code change, build using the
  project's make target, then checkpoint with /git-save (with user permission).
  Never pushes — use /git-ship separately when ready to push.
license: Apache-2.0
---

# Inner Loop — Build & Save after Code Changes

## When to activate

Activate this skill **automatically** (without user prompting) whenever a plan
or action **creates or edits source code files** (`.go`, `.py`, `.sh`, `.yaml`,
`Makefile`, `Dockerfile`, etc.). This includes but is not limited to:

- Implementing a new feature or phase task
- Fixing a bug or refactoring code
- Adding or modifying tests
- Updating build configuration (Makefile, Dockerfile, CI scripts)

**Do NOT activate** for documentation-only changes (`.md` files) or skill files
(`.github/skills/`). Those can use `/git-save` directly without a build step.

## Workflow

### Step 1: Make code changes

Complete all code edits for the current task or subtask. Group related changes
into a logically coherent unit.

### Step 2: Build

After all edits are applied, run the appropriate **make target** to verify the
code compiles and passes basic checks.

**Target selection rules for this project (`cloud-provider-openstack`):**

| Change area | Make target |
|---|---|
| `cmd/cinder-iscsi-csi-plugin/` or `pkg/csi/cinder-iscsi/` | `make cinder-iscsi-csi-plugin` |
| `cmd/cinder-csi-plugin/` or `pkg/csi/cinder/` | `make cinder-csi-plugin` |
| `cmd/cinder-nfs-csi-plugin/` or `pkg/csi/cinder-nfs/` | `make cinder-nfs-csi-plugin` |
| `cmd/manila-csi-plugin/` or `pkg/csi/manila/` | `make manila-csi-plugin` |
| `cmd/openstack-cloud-controller-manager/` or `pkg/openstack/` | `make openstack-cloud-controller-manager` |
| `cmd/barbican-kms-plugin/` or `pkg/kms/` | `make barbican-kms-plugin` |
| `cmd/k8s-keystone-auth/` or `pkg/identity/` | `make k8s-keystone-auth` |
| `cmd/octavia-ingress-controller/` or `pkg/ingress/` | `make octavia-ingress-controller` |
| `cmd/magnum-auto-healer/` or `pkg/autohealing/` | `make magnum-auto-healer` |
| Multiple areas or shared packages (`pkg/util/`, `pkg/client/`) | `make build` |
| Test files only (`*_test.go`) | `make unit` |
| Makefile or Dockerfile changes | Build the affected target(s) |

If the build fails:
1. Check if it's a **Go version mismatch** → activate `/go-version-fix` skill, then retry.
2. Otherwise, **fix the error** in code and re-run the build.
3. Repeat until the build succeeds.

**Do NOT proceed to Step 3 until the build passes.**

### Step 3: Save (with permission)

Once the build succeeds, **ask the user for permission** before committing:

Use `ask_questions` with a single question:
- Header: `git-save`
- Question: `Build succeeded. Save checkpoint with /git-save?`
- Options:
  - `Yes, save` (recommended)
  - `Skip save`

**If the user approves:** Execute the `/git-save` skill (stage + commit, no push).
**If the user declines:** Skip the commit. Changes remain unstaged.

## Rules

- **NEVER use `/git-ship`** in the inner loop. No pushing.
- **NEVER skip the build step** for code changes.
- **ALWAYS ask permission** before running `/git-save`.
- If the user explicitly says "don't save" or "skip save" at any point, respect it.
- The build step is skipped only for non-code changes (docs, skills, configs).

````
