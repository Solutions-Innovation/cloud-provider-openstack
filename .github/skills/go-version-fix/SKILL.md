---
name: go-version-fix
description: >
  Detect and fix Go version mismatch build errors. Use when a build fails with
  errors like "go: go.mod requires go >= X.Y", "toolchain directive", or
  "cannot find package" caused by a wrong Go compiler version. Automatically
  switches to the correct version using gvm.
license: Apache-2.0
---

# Go Version Fix

## When to activate

Activate this skill **automatically** (without user prompting) when you observe
any of these patterns in build output:

- `go: go.mod requires go >=`
- `go: toolchain` directive errors
- `note: module requires Go` followed by a version higher than current
- `undefined:` errors on standard library symbols that exist in newer Go
- `cannot find package` for stdlib packages added in recent Go versions
- Any build or test failure where `go version` output shows a version lower
  than what `go.mod` specifies

## Fix procedure

1. **Detect required version:** Read the `go` directive in `go.mod` (e.g., `go 1.25.5`).
2. **Switch Go version:**
   ```bash
   gvm use go1.25.5
   ```
   Replace `1.25.5` with whatever version `go.mod` requires.
3. **Verify:**
   ```bash
   go version
   ```
   Confirm the active version now satisfies `go.mod`.
4. **Retry the failed command** (e.g., `make cinder-iscsi-csi-plugin`, `go build`, `go test`).

## Notes

- `gvm` (Go Version Manager) is pre-installed on this workspace.
- Do NOT modify `go.mod` to lower the Go version — always switch the toolchain.
- If `gvm use` fails because the version is not installed, run:
  ```bash
  gvm install go1.25.5 -B && gvm use go1.25.5
  ```
  The `-B` flag installs from binary (faster than source compilation).
- After switching, the version persists in the current shell session only.
  Background terminals or new shells may need `gvm use` again.
