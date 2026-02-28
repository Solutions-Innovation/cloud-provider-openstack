---
name: git-save
description: >
  Stage and commit all current changes with an auto-generated conventional-commit
  message. Use when the user says "save", "commit", "git-save", "checkpoint", or
  similar. Does NOT push — use /git-ship to commit and push.
license: Apache-2.0
---

# Git Save — Stage & Commit (no push)

## Workflow

### Step 1: Gather context

1. Use `mcp_gitkraken_git_status` to see the working tree state.
2. If there are no changes, inform the user and stop.

### Step 2: Generate commit message

Analyze the changed files and produce a **conventional-commit** message:

```
<type>(<scope>): <subject>
```

Rules:
- **type**: `feat`, `fix`, `docs`, `refactor`, `test`, `chore`, `build`, `ci`
- **scope**: short area token (e.g., `iscsi-csi`, `nfs-csi`, `helm`). Omit if cross-cutting.
- **subject**: imperative mood, lowercase, no period, max 72 chars. Be specific.
- **body**: only if >50 lines changed or multiple logical changes. Bullet points, ≤5 lines.

### Step 3: Stage & Commit

1. Use `mcp_gitkraken_git_add_or_commit` with `action: "add"` to stage all changes.
2. Use `mcp_gitkraken_git_add_or_commit` with `action: "commit"` and the message.
3. Report: commit hash, file count, one-line summary.

No review gate — this is a fast local checkpoint. No push.
