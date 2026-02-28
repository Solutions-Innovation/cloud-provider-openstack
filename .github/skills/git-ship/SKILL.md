---
name: git-ship
description: >
  Stage all changes, generate a concise conventional-commit message from the diff,
  commit, and push to the remote branch. Use when the user says "ship", "commit and
  push", "git-ship", "push my changes", or similar. Always asks for review before
  pushing to remote.
license: Apache-2.0
---

# Git Ship — Stage, Commit & Push

## Workflow

Follow these steps **exactly in order**. Do NOT skip the review step.

### Step 1: Gather context

1. Use `mcp_gitkraken_git_status` to see the working tree state.
2. Use `get_changed_files` to obtain the full diffs of unstaged and staged changes.
3. If there are no changes at all, inform the user and stop.

### Step 2: Generate commit message

Analyze the diffs and produce a **conventional-commit** message:

```
<type>(<scope>): <subject>

<body>
```

Rules for the commit message:
- **type**: `feat`, `fix`, `docs`, `refactor`, `test`, `chore`, `build`, `ci` — pick
  the most appropriate one. If changes span multiple types, use the dominant one.
- **scope**: short token for the area changed (e.g., `iscsi-csi`, `nfs-csi`, `occm`,
  `helm`, `docs`). Omit scope if changes are truly cross-cutting.
- **subject**: imperative mood, lowercase, no period, max 72 chars.
  Summarize *what* changed, not *how*. Be specific — avoid vague phrases like
  "update files" or "make changes".
- **body** (optional): add a body only if the diff is non-trivial (>50 lines changed
  or multiple logical changes). Use bullet points. Keep it under 5 lines.
  Wrap at 72 chars.

Examples of good subjects:
- `docs(iscsi-csi): add block-only volume mode enforcement section`
- `feat(nfs-csi): implement NodeStageVolume with NFS mount`
- `fix(occm): handle nil pointer in route controller`
- `chore: update gophercloud to v2.8.0`

Examples of bad subjects:
- `update docs` (too vague)
- `Changes to implementation design` (not imperative, not specific)
- `Added new feature.` (past tense, has period)

### Step 3: Review before commit

Present the proposed commit to the user using `ask_questions`:

- Show: file summary (from git status), the full proposed commit message
- Question: "Proceed with commit and push?" with options:
  - **Commit & push** (recommended)
  - **Edit message** — let user provide a modified message via freeform input
  - **Commit only** — commit but do not push
  - **Abort** — do nothing

If the user selects "Edit message", use their revised text and re-confirm.

### Step 4: Stage & Commit

1. Use `mcp_gitkraken_git_add_or_commit` with `action: "add"` to stage all changes.
2. Use `mcp_gitkraken_git_add_or_commit` with `action: "commit"` and the approved
   message.

If either step fails, report the error and stop.

### Step 5: Push (if approved)

If the user approved pushing:

1. Use `mcp_gitkraken_git_push` to push to the remote.
2. Report success with the branch name and a one-line summary.

If push fails, diagnose the error:

- **Upstream rejected (non-fast-forward):** suggest `git pull --rebase` before retrying.
- **SSH authentication failure** (e.g., `Permission denied (publickey)`,
  `Could not read from remote repository`): Run the following in the terminal to load
  the SSH key, then retry the push:
  ```bash
  eval $(ssh-agent) && ssh-add ~/.ssh/wr56
  ```
  After the key is loaded, retry `mcp_gitkraken_git_push`. If it still fails, report
  the error and stop.

## Edge cases

- **Merge conflicts:** If `get_changed_files` with `sourceControlState: ["merge-conflicts"]`
  returns results, warn the user to resolve conflicts first and stop.
- **Detached HEAD:** If git status shows detached HEAD, warn the user and stop.
- **Nothing to commit:** If working tree is clean, say so and stop.
- **Untracked files only:** Include them — `git add` stages untracked files too.
  Mention them explicitly in the review step so the user is aware.
- **Large diffs (>500 lines):** Still generate a commit message, but keep it high-level.
  Mention the number of files changed and the primary areas affected.
