```skill
---
name: skill-authoring
description: >
  Guide for creating, refactoring, and validating agent skills in this
  repository. Encodes the Agent Skills open specification (agentskills.io)
  rules for directory layout, SKILL.md format, frontmatter fields,
  progressive disclosure, file size limits, and naming conventions. Activate
  when the user wants to create a new skill, refactor an existing skill, or
  review a skill for spec compliance.
license: Apache-2.0
---

# Skill Authoring Guide

## When to activate

Activate this skill when the user says any of:
- "create a new skill", "add a skill", "new agent skill"
- "refactor skill", "split skill", "skill too large"
- "check skill spec", "validate skill", "skill compliance"
- Any reference to `.github/skills/` structure or SKILL.md authoring

Do NOT activate for domain-specific skill execution — each skill activates itself.

## Skill location in this repository

All skills live under `.github/skills/<skill-name>/`. The skill loader reads
the `SKILL.md` in each directory. This repo currently has these skills:

| Skill | Purpose |
|-------|---------|
| `cloudify-migration` | V2O migration e2e via Cloudify CLI |
| `dev-deploy` | Build, push, deploy CSI plugin to staging |
| `doc-align` | Align documentation with code changes |
| `git-save` | Quick commit + push workflow |
| `git-ship` | Full PR/merge workflow |
| `go-version-fix` | Fix Go version mismatches |
| `inner-loop` | Local build + test loop |
| `nfs-cinder-csi` | NFS-Cinder CSI plugin development |
| `skill-authoring` | This skill — meta-guide for authoring skills |

Update this table when adding or removing skills.

## Directory structure (required)

Every skill is a directory with at minimum a `SKILL.md` file:

```
skill-name/
└── SKILL.md          # Required — frontmatter + instructions
```

### Optional directories

Add these only when needed. Keep file references **one level deep** from SKILL.md.

```
skill-name/
├── SKILL.md
├── references/       # Docs loaded on demand (REFERENCE.md, domain files, etc.)
├── scripts/          # Executable code the agent can run (Python, Bash, etc.)
└── assets/           # Static resources (templates, configs, schemas, data files)
```

**`references/`** — Additional documentation the agent reads when it needs
deeper context. Keep individual files focused; agents load these on demand so
smaller files mean less context usage. Examples: `architecture.md`,
`cdi-lifecycle.md`, `input-templates.md`, `monitoring.md`.

**`scripts/`** — Self-contained executable scripts with clear error messages.
Supported languages depend on the agent implementation.

**`assets/`** — Static resources: YAML templates, config files, lookup tables,
schemas, images. Example: `e2e-iscsi-defaults.yaml`.

### Existing repo examples

Skills already following this structure:

- **`cloudify-migration/`** — `references/` (cdi-lifecycle.md, input-templates.md,
  monitoring.md) + `assets/` (e2e-iscsi-defaults.yaml)
- **`nfs-cinder-csi/`** — `references/` (architecture.md, build-deploy.md,
  csi-rpcs.md, interfaces.md, shadow-vm.md)

## SKILL.md format

### Frontmatter (required)

YAML frontmatter delimited by `---`. Must appear at the very top of SKILL.md
(inside the ` ```skill ` fence used by this repo's loader).

```yaml
---
name: skill-name
description: >
  What this skill does and when to use it. Include specific keywords
  that help agents identify relevant tasks.
license: Apache-2.0
---
```

#### Field rules

| Field | Required | Constraints |
|-------|----------|------------|
| `name` | **Yes** | 1–64 chars. Lowercase `a-z`, digits `0-9`, hyphens `-` only. Must not start/end with `-`. No consecutive `--`. **Must match parent directory name.** |
| `description` | **Yes** | 1–1024 chars. Describe what the skill does AND when to use it. Include keywords for agent matching. |
| `license` | No | License name or ref to bundled file. Use `Apache-2.0` for this repo. |
| `compatibility` | No | 1–500 chars. Only if the skill has environment requirements. |
| `metadata` | No | Key-value map for additional properties. |
| `allowed-tools` | No | Space-delimited pre-approved tool list. Experimental. |

**Name validation examples:**
- `pdf-processing` — valid
- `PDF-Processing` — **invalid** (uppercase)
- `-pdf` — **invalid** (starts with hyphen)
- `pdf--processing` — **invalid** (consecutive hyphens)

**Description quality:**
- Good: `"Extracts text and tables from PDF files, fills PDF forms, and merges multiple PDFs. Use when working with PDF documents or when the user mentions PDFs, forms, or document extraction."`
- Poor: `"Helps with PDFs."`

### Body content

The Markdown body after frontmatter contains skill instructions. No format
restrictions — write whatever helps agents perform the task effectively.

**Recommended sections:**
1. **When to activate** — trigger phrases and exclusions
2. **Environment setup** — prerequisites, CLI tools, env vars
3. **Workflow / Steps** — step-by-step instructions
4. **Rules** — ALWAYS/NEVER directives
5. **References** — table pointing to `references/` and `assets/` files

## Progressive disclosure

Structure skills for efficient context usage. This is the core design principle:

```
Layer 1 — Metadata (~100 tokens)
  name + description loaded at startup for ALL skills (agent decides which to activate)

Layer 2 — Instructions (< 5000 tokens recommended)
  Full SKILL.md body loaded when the skill activates

Layer 3 — Resources (on demand)
  Files in references/, scripts/, assets/ loaded only when the agent needs them
```

### Size limits

| Layer | Recommended limit | Action if exceeded |
|-------|-------------------|-------------------|
| SKILL.md body | **< 500 lines** | Extract reference material to `references/` |
| Individual reference file | < 300 lines | Split into focused subtopics |
| Description field | ≤ 1024 chars | Shorten — it's for matching, not full docs |

### How to reference files

Use relative paths from the skill root directory:

```markdown
See [references/architecture.md](references/architecture.md) for details.

Run the setup script:
scripts/setup.sh

Use the pre-built config from [assets/defaults.yaml](assets/defaults.yaml).
```

**Rules:**
- Keep references **one level deep** from SKILL.md — avoid deeply nested chains
- Use a **References table** in SKILL.md to catalog all referenced files with
  a "When to read" column so the agent knows when to load each file

Example references table:

```markdown
## References

| File | Content | When to read |
|------|---------|-------------|
| [references/architecture.md](references/architecture.md) | System design, data flow | Understanding codebase structure |
| [references/monitoring.md](references/monitoring.md) | CLI commands, debugging | Troubleshooting failures |
| [assets/defaults.yaml](assets/defaults.yaml) | Pre-built config | Fast-path setup |
```

## Creating a new skill — step by step

### 1. Choose a name

Pick a lowercase, hyphenated name that describes the skill's domain:
- `my-new-skill` — valid
- Must match the directory name exactly

### 2. Create the directory

```bash
mkdir -p .github/skills/<skill-name>
```

### 3. Write SKILL.md

Start from the template in [assets/SKILL-TEMPLATE.md](assets/SKILL-TEMPLATE.md):

```bash
cp .github/skills/skill-authoring/assets/SKILL-TEMPLATE.md \
   .github/skills/<skill-name>/SKILL.md
```

Then edit the frontmatter and body.

### 4. Add references/ and assets/ if needed

Only add these directories if the skill content exceeds ~300 lines or there are
static resources to include. For small skills (< 200 lines), keep everything in
SKILL.md.

### 5. Verify line count

```bash
wc -l .github/skills/<skill-name>/SKILL.md
# Must be < 500 lines
```

### 6. Update the skills table

Add the new skill to the table in **this file** (skill-authoring/SKILL.md,
"Skill location" section) so future skill discovery is accurate.

## Refactoring an existing skill

When an existing SKILL.md exceeds 500 lines:

1. **Identify extractable content** — reference material, templates, monitoring
   commands, debugging guides, edge cases, large code examples
2. **Create `references/`** directory and move extracted content to focused `.md` files
3. **Create `assets/`** directory for static resources (YAML templates, configs)
4. **Replace inline content** with relative-path references in SKILL.md
5. **Add a References table** to SKILL.md cataloging all extracted files
6. **Verify** SKILL.md is under 500 lines

## Skill quality checklist

When creating or reviewing a skill, verify:

- [ ] `name` in frontmatter matches directory name
- [ ] `name` is lowercase, hyphenated, 1–64 chars, no leading/trailing/consecutive hyphens
- [ ] `description` clearly states what + when, ≤ 1024 chars
- [ ] SKILL.md has "When to activate" section with trigger phrases
- [ ] SKILL.md has "Do NOT activate for" exclusions where relevant
- [ ] SKILL.md is under 500 lines
- [ ] Reference files are under 300 lines each
- [ ] All file references use relative paths from skill root
- [ ] References are one level deep (no nested chains)
- [ ] A References table exists if the skill has `references/` or `assets/`
- [ ] `license: Apache-2.0` is set (for this repo)
- [ ] Skills table in this file is updated

## Rules

- **ALWAYS follow the Agent Skills spec** (agentskills.io/specification) for structure and naming
- **ALWAYS keep SKILL.md under 500 lines** — extract to `references/` when exceeding
- **ALWAYS use progressive disclosure** — metadata → instructions → on-demand resources
- **ALWAYS match directory name to frontmatter `name` field** exactly
- **ALWAYS use lowercase + hyphens** for skill names — no uppercase, underscores, or spaces
- **ALWAYS include a "When to activate" section** with specific trigger phrases
- **ALWAYS add a References table** when using `references/` or `assets/` directories
- **NEVER nest file references** — keep them one level deep from SKILL.md
- **NEVER put large code blocks, templates, or lookup tables inline** in SKILL.md if they push it over 300 lines — extract to references or assets
- **NEVER create empty optional directories** — only add `references/`, `scripts/`, `assets/` when there is content to put in them
```
