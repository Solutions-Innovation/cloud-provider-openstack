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

## Copyright Header Rule

Every **new** `.go` file created by this team MUST use the Wind River copyright
header. Do NOT use the Kubernetes Authors header for new files.

**Canonical Go copyright header:**

```go
/*
Copyright (c) 2024-2026 Wind River Systems, Inc.
Wind River Migration Framework Team

SPDX-License-Identifier: Apache-2.0

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
```

**Rules:**
- The copyright year range starts at the file's creation year and ends at the
  current year (e.g., `2024-2026`). If created and modified in the same year,
  use a single year (e.g., `2026`).
- `SPDX-License-Identifier: Apache-2.0` is mandatory — it enables automated
  license scanning tools.
- **Do NOT modify** headers in files owned by upstream Kubernetes (e.g.,
  `pkg/csi/cinder/`, `pkg/openstack/`). Only our new files under
  `pkg/csi/cinder-iscsi/`, `cmd/cinder-iscsi-csi-plugin/`, and similar
  Wind River-authored paths use this header.
- Files **copied** from upstream (e.g., `server.go`, `utils.go`) that are then
  substantially modified should use a **dual header**: keep the original
  Kubernetes copyright and add a Wind River copyright block below it.

**Dual header for copied-and-modified files:**

```go
/*
Copyright 2017 The Kubernetes Authors.
(original upstream copyright preserved)

Copyright (c) 2024-2026 Wind River Systems, Inc.
Wind River Migration Framework Team
Modifications: <brief description of changes>

SPDX-License-Identifier: Apache-2.0

Licensed under the Apache License, Version 2.0 ...
*/
```

## Workflow

### Step 1: Make code changes

Complete all code edits for the current task or subtask. Group related changes
into a logically coherent unit. Ensure all new `.go` files use the Wind River
copyright header (see above).

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
