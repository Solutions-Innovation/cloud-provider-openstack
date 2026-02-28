```skill
---
name: doc-align
description: >
  Audit design and implementation docs against recent code changes and update them
  to stay in sync. Use when the user says "align docs", "doc-align", "sync docs",
  "update design doc", or after implementing a feature/flag that may not be reflected
  in the design or implementation design documents.
license: Apache-2.0
---

# Doc Align — Keep Design & Implementation Docs in Sync with Code

## Purpose

Code changes often introduce new config fields, feature flags, struct changes,
new RPCs, or behavioral changes that are not reflected in the design doc or the
implementation design doc. This skill performs a structured audit and applies
targeted updates so the docs stay authoritative.

## When to use this skill

- After adding a new config option or feature flag to a struct (e.g., `ISCSIOpts`,
  `VolumeOpts`)
- After adding or changing an RPC's behavior (e.g., new precedence logic in
  `DeleteVolume`)
- After adding a new interface method to `IOpenStackISCSI` or `IOpenStackNFS`
- After adding new constants, types, or data structures
- After any refactor that changes the public contract visible in the design docs
- When the user says "align docs", "doc-align", "sync docs with code"

## Document map

The skill supports multiple driver contexts. Determine which documents to audit
based on the changed files:

### iSCSI-Cinder CSI Driver

| Document | Path | Sections to audit |
|----------|------|-------------------|
| **Design doc** | `docs/cinder-csi-plugin/migration/iscsi-backed-cinder-volume-for-wrcp-migration.md` | §7.1 Driver Config Reference, §5 CSI RPC Mapping, §9 Prerequisites, §10 Risks |
| **Impl design doc** | `docs/cinder-csi-plugin/migration/iscsi-cinder-csi-implementation-design.md` | §6.2 Config Structs, §6.1 Interface, §7 Controller RPCs, §8 Node RPCs, pseudocode |

### NFS-Cinder CSI Driver

| Document | Path | Sections to audit |
|----------|------|-------------------|
| **Design doc** | `docs/cinder-csi-plugin/migration/nfs-backed-cinder-volume-for-wrcp-migration.md` | Config, CSI RPC mapping, Prerequisites |
| **Impl design doc** | `docs/cinder-csi-plugin/migration/nfs-cinder-csi-implementation-design.md` | Config structs, Interface, RPC pseudocode |

## Workflow

Follow these steps in order. Be systematic — do not skip the audit phase.

### Step 1: Identify what changed

1. Use `mcp_gitkraken_git_status` to see uncommitted changes.
2. Use `mcp_gitkraken_git_log_or_diff` to review recent commits (since last doc-align
   or the last 5 commits, whichever is smaller).
3. Categorize each change into one of these **change types**:

| Change Type | Example | Doc Impact |
|-------------|---------|------------|
| **New config field** | Added `DeleteVolumeMode` to `VolumeOpts` | Config struct in impl doc, config reference table in design doc, `driver.conf` example in both |
| **New interface method** | Added `DiscoverCinderCapabilities()` | Interface listing in impl doc, capability section in design doc |
| **New constant** | Added `DeleteVolumeModeRetain` | Impl doc constants section |
| **RPC behavior change** | `DeleteVolume` now reads driver config | Pseudocode in impl doc, RPC section in design doc, flow diagrams |
| **New connector field** | `Platform`, `OSType` now configurable | Config struct, connector pseudocode, `driver.conf` example |
| **New metadata key** | New volume metadata key | Metadata keys table in impl doc |
| **New test** | Added `TestDeleteVolume_DriverConfigDelete` | Usually no doc update needed unless it validates a new feature |
| **Refactor (no behavior change)** | Extracted `blockStorageClient()` helper | Usually no doc update unless public API changed |

4. Build a checklist of doc sections that need updating.

### Step 2: Audit — compare code vs docs

For each change type identified in Step 1, compare the **current code** against
**both documents**. Use `grep_search` and `read_file` to check:

#### Config fields audit

```
Code source:    openstack.go → ISCSIOpts / VolumeOpts structs
Design doc:     §7.1 config tables ([ISCSI] and [Volume] sections)
Impl doc:       §6.2 struct definitions + driver.conf example
```

Check for:
- [ ] Every `gcfg` tag in the code struct has a row in the design doc config table
- [ ] Every field has correct type, default, and description
- [ ] The `driver.conf` example in BOTH docs includes all fields
- [ ] If a field has special semantics (precedence, override), it is documented

#### Interface audit

```
Code source:    openstack.go → IOpenStackISCSI interface
Impl doc:       §6.1 interface listing
```

Check for:
- [ ] Every method on the interface is listed in the doc
- [ ] Method signatures match (params + return types)

#### RPC behavior audit

```
Code source:    controllerserver.go / nodeserver.go → RPC functions
Design doc:     §5.2 / §5.3 RPC sections
Impl doc:       §7 / §8 pseudocode blocks
```

Check for:
- [ ] Doc pseudocode matches the actual implementation logic
- [ ] Flow step numbering matches (e.g., "Step 3: Resolve effective delete mode")
- [ ] Comments in pseudocode reflect actual behavior

#### Connector fields audit

```
Code source:    controllerserver.go → AttachmentConnector construction
Impl doc:       §6.2 AttachmentConnector struct + ControllerPublishVolume pseudocode
Design doc:     §5.2.2 connector JSON examples
```

Check for:
- [ ] All connector fields in code are shown in the doc examples
- [ ] Hardcoded vs configurable distinction is accurate

### Step 3: Report gaps

Present findings to the user as a table:

```markdown
| # | Gap | Code Location | Doc | Section | Action |
|---|-----|---------------|-----|---------|--------|
| 1 | `Platform` field missing from ISCSIOpts | openstack.go:149 | impl | §6.2 | Add field |
| 2 | driver.conf missing [ISCSI] section | — | impl | §6.2 | Expand example |
| 3 | No config reference section | — | design | §7 | Add §7.1 |
```

### Step 4: Apply updates

After the user confirms (or if they said "align docs" which implies auto-fix):

1. Update impl design doc first (it is the source of truth for code structure).
2. Update main design doc second (it references the impl doc).
3. Update table of contents if new sections were added.
4. Ensure cross-references between docs are correct.

**Style rules for doc updates:**
- Match the existing document's formatting, heading levels, and table style.
- Config tables: `| Key | Type | Default | Description |` format.
- Struct definitions: show the Go struct with `gcfg` tags and inline comments.
- Pseudocode: use the existing indented `├──` / `│` flow style if present.
- `driver.conf` examples: show ALL fields with their defaults, use INI format.

### Step 5: Save

Use the `/git-save` skill to commit doc updates with type `docs`.

## Anti-patterns to avoid

- **Don't update docs for every test addition** — test names don't belong in design docs.
- **Don't duplicate code in docs** — pseudocode should convey intent, not be copy-paste.
- **Don't add doc sections for internal refactors** — only public contract changes.
- **Don't remove doc content that describes future work** — flag it as completed instead.
- **Don't update one doc without checking the other** — always audit both.

## Quick reference: what goes where

| Content | Design Doc | Impl Doc |
|---------|-----------|----------|
| Config option table (user-facing) | ✅ §7.1 | ✅ §6.2 |
| Go struct definition | ❌ | ✅ §6.2 |
| driver.conf example | ✅ §7.1 | ✅ §6.2 |
| Interface method list | ❌ | ✅ §6.1 |
| RPC pseudocode | ❌ (high-level flow only) | ✅ §7/§8 |
| RPC behavior description | ✅ §5.2/§5.3 | ✅ §7/§8 |
| Architecture diagrams | ✅ §3 | ❌ |
| Precedence / override rules | ✅ §7.1 (note) | ✅ §6.2 (full detail) |
| Kubernetes manifests | ❌ | ✅ §5 |
| Prerequisites | ✅ §9 | ❌ |
| Risks & mitigations | ✅ §10 | ❌ |

```
