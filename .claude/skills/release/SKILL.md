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

Parse the commit type prefix (`feat(`, `fix(`, `chore(`, etc.) from every commit
since the last tag. The bump is determined mechanically:

- **patch** (`v0.X.Y+1`): all commits are `fix`, `chore`, `docs`, `refactor`, or `test`
- **minor** (`v0.X+1.0`): at least one `feat` commit
- **major** (`vX+1.0.0`): only when the user explicitly requests it (breaking changes)

To determine the bump, run:

```bash
# Check if any feat commits exist since last tag
git log $(git describe --tags --abbrev=0)..HEAD --oneline | grep -c '^[a-f0-9]* feat('
```

If count > 0 → minor. Otherwise → patch. Never auto-suggest major.

If the user specified a version, use that. Otherwise present the suggested bump
with the reasoning (e.g. "3 feat commits → minor bump").

## Step 3: Collect bead context

For each unique bead ID referenced in commits, fetch the bead title:

```bash
bd show <bead-id> --json 2>/dev/null | python3 -c "import json,sys; b=json.load(sys.stdin); print(b.get('title',''))"
```

Group commits by bead ID — multiple commits for the same bead become one bullet.

## Step 4: Draft release notes

Group by section. Format:

```markdown
## Features
- **<short description>** — <detail from bead/commit> (`<bead-id>`)

## Fixes
- **<short description>** — <detail> (`<bead-id>`)

## Improvements
- <chore/refactor/docs changes, grouped if minor>

## Internal
- <test changes, CI, dependency bumps — only if noteworthy>
```

Rules:
- Lead with the user-facing impact, not the code change
- Combine related commits under one bullet (e.g. multiple commits for one bead)
- Skip trivial chores unless they affect users (dep upgrades, migration fixes)
- Bead IDs link context — always include them
- Keep it concise: aim for 5-15 bullets total, not one per commit
- Omit empty sections

## Step 5: Present for review

Show the user:
1. The version bump with reasoning (e.g. `v0.33.0 → v0.34.0 (minor: 3 feat commits)`)
2. The draft release notes
3. Ask: "Look good? I'll write the notes, commit, tag, and push."

Wait for confirmation before proceeding. The user may want to edit the notes
or override the version.

## Step 6: Write notes, commit, push

After user approval:

1. Write release notes to `releases/<version>.md` (versioned for posterity)
2. Commit the notes file
3. Push to main — CI runs tests, detects the unreleased notes file, tags, and
   goreleaser builds and publishes the release

```bash
# Write release notes
cat > releases/<version>.md <<'EOF'
<release notes here>
EOF

# Commit and push
git add releases/<version>.md
git commit -m "docs: release notes for <version>"
git push origin main
```

CI detects unreleased versions by finding `releases/v*.md` files that don't
have a corresponding git tag. When CI passes and an untagged notes file exists,
it tags the commit and runs goreleaser automatically.

IMPORTANT:
- Never use `gh release create` — goreleaser creates the release.
- Never manually create tags — CI creates the tag after tests pass.
- Never use `git tag` locally for releases.
- Release notes files are permanent — `releases/v0.34.0.md`, `releases/v0.35.0.md`, etc.
- The presence of an untagged notes file IS the release trigger.

## Edge cases

- If there are uncommitted changes, warn the user and stop
- If HEAD is already tagged, tell the user — nothing to release
- If the user asks for a pre-release, use `-rc.1` suffix
- If commits span multiple bead IDs, group by bead not by commit
- If the user overrides the version, use their version without argument
