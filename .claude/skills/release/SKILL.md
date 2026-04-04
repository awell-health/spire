---
name: release
description: Tag a new Spire release with structured release notes. Use when the user says "/release", "tag a release", "cut a release", or "release notes". Determines version bump (major/minor/patch) from commit types since last tag.
---

# Spire Release

Create a tagged release with structured release notes derived from the commit
history and bead graph.

## Step 1: Gather context

Run these commands to understand what's changed:

```bash
# Latest tag
git describe --tags --abbrev=0

# Commits since last tag (type, bead, message)
git log $(git describe --tags --abbrev=0)..HEAD --oneline

# Current version for bump suggestion
git tag --sort=-v:refname | head -5
```

## Step 2: Determine version bump

Based on commits since last tag:
- **patch** (v0.X.Y+1): only `fix`, `chore`, `docs`, `refactor`, `test`
- **minor** (v0.X+1.0): any `feat` commits
- **major** (vX+1.0.0): breaking changes (rare, user must confirm)

If the user specified a version, use that. Otherwise suggest the appropriate bump.

## Step 3: Collect bead context

For each bead ID referenced in commits, fetch the bead title:

```bash
bd show <bead-id> --json 2>/dev/null | python3 -c "import json,sys; b=json.load(sys.stdin); print(b.get('title',''))"
```

## Step 4: Draft release notes

Group commits into sections. Format:

```markdown
## What's new

### Features
- **<short description>** — <detail from bead/commit> (`<bead-id>`)

### Fixes
- **<short description>** — <detail> (`<bead-id>`)

### Improvements
- <chore/refactor/docs changes, grouped if minor>

### Internal
- <test changes, CI, dependency bumps — only if noteworthy>
```

Rules:
- Lead with the user-facing impact, not the code change
- Combine related commits under one bullet (e.g. multiple commits for one bead)
- Skip trivial chores unless they affect users (dep upgrades, migration fixes)
- Bead IDs link context — always include them
- Keep it concise: aim for 5-15 bullets total, not one per commit

## Step 5: Present for review

Show the user:
1. The suggested version (e.g. `v0.32.1 -> v0.33.0`)
2. The draft release notes
3. Ask: "Look good? I'll tag, push, and create the GitHub release."

Wait for confirmation before proceeding. The user may want to edit.

## Step 6: Tag and publish

After user approval:

```bash
# Tag
git tag <version>

# Push commit and tag
git push origin main --tags

# Create GitHub release
gh release create <version> --title "<version>" --notes "$(cat <<'EOF'
<release notes here>
EOF
)"
```

## Edge cases

- If there are uncommitted changes, warn the user and stop
- If HEAD is already tagged, tell the user — nothing to release
- If the user asks for a pre-release, use `-rc.1` suffix
- If commits span multiple bead IDs, group by bead not by commit
