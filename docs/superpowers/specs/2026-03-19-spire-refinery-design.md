# Spire Refinery — Opus-Powered PR Validation & Learning System

**Date**: 2026-03-19
**Status**: Design

## The model split

| Role | Model | Why |
|------|-------|-----|
| Worker | Sonnet | Fast, cheap, good at implementation. $1-3/task. |
| Refinery | Opus (1M context) | Deep reasoning, can hold the full spec + all diffs + test results. Reviews like a senior engineer. |
| Mayor | None (or Haiku for routing) | Pure coordination, no LLM needed for basic assignment. |

The refinery is the senior engineer. Workers are junior devs.
The refinery never writes code — it reads code and judges it.

## What the refinery does

### 1. Validate work against spec

When a worker pushes a branch:

```
Refinery reads:
  - The spec (full document)
  - The bead description
  - The diff (git diff main...feat/<bead-id>)
  - Test results
  - Any linting/build output

Refinery asks (using Opus 1M):
  "Does this implementation satisfy the spec?
   Is the code correct, clean, and complete?
   Are there edge cases not handled?
   Does it match the architectural intent?"

Output:
  - APPROVE → create PR, mark as ready for human review
  - REQUEST_CHANGES → send feedback to worker, worker revises
  - REJECT → close the attempt, report to mayor, maybe re-spec
```

### 2. Request changes from workers

When the refinery finds issues, it doesn't fix them. It sends
a structured review back to the worker:

```json
{
  "verdict": "request_changes",
  "issues": [
    {
      "file": "src/auth/oauth.ts",
      "line": 42,
      "severity": "error",
      "message": "Missing error handling for token refresh failure. The spec requires graceful degradation to session-based auth."
    },
    {
      "file": "src/auth/oauth.test.ts",
      "severity": "warning",
      "message": "No test for the token expiry path. Add a test that verifies refresh behavior."
    }
  ],
  "summary": "Core implementation is correct but missing error handling for the token refresh failure case specified in section 3 of the spec."
}
```

This gets written to `/comms/review.json` and the sidecar sends it
to the worker as a `STEER:` message. The worker reads the review,
makes changes, pushes again. The refinery re-reviews.

**Max review rounds**: configurable (default 3). After 3 rounds of
changes, the refinery escalates to the human with a summary of
what's still wrong.

### 3. Track context usage

Every worker run and every refinery review records:

```json
{
  "bead_id": "spi-abc.2",
  "agent": "worker-3",
  "model": "claude-sonnet-4-6",
  "context_tokens_in": 45000,
  "context_tokens_out": 12000,
  "total_tokens": 57000,
  "turns": 23,
  "duration_seconds": 180,
  "result": "success",
  "review_rounds": 1,
  "refinery_verdict": "approve",
  "spec_file": "docs/superpowers/specs/2026-03-19-onboarding.md",
  "spec_size_tokens": 3200,
  "focus_context_tokens": 8500,
  "files_changed": 4,
  "lines_added": 120,
  "lines_removed": 15,
  "tests_added": 3,
  "tests_passed": true
}
```

This gets stored in the beads database (new `agent_runs` table)
and is queryable:

```bash
# How much context do tasks in this repo typically need?
bd sql "SELECT avg(context_tokens_in), avg(turns), avg(review_rounds)
        FROM agent_runs WHERE result='success'"

# Which specs produce the cleanest first-pass implementations?
bd sql "SELECT spec_file, avg(review_rounds), count(*)
        FROM agent_runs
        GROUP BY spec_file
        ORDER BY avg(review_rounds)"

# What's our success rate by bead size?
bd sql "SELECT
          CASE WHEN lines_added < 50 THEN 'small'
               WHEN lines_added < 200 THEN 'medium'
               ELSE 'large' END as size,
          count(*) as total,
          sum(CASE WHEN result='success' THEN 1 ELSE 0 END) as succeeded,
          avg(total_tokens) as avg_tokens
        FROM agent_runs
        GROUP BY size"
```

### 4. Learn optimal task sizing

Over time, the data tells you:

**"Tasks under 200 lines succeed 92% of the time with 1.1 review rounds.
Tasks over 500 lines succeed 61% of the time with 2.8 review rounds."**

This feeds back into spec writing and bead decomposition:

```bash
spire spec --break spi-abc
# Spire reads the spec, estimates implementation size per section,
# and suggests a breakdown that keeps each child bead in the
# "high success" zone (< 200 lines, < 50K context tokens)
```

The `spire spec --break` command can use the historical data:

```
Breaking epic spi-abc into tasks...

  spi-abc.1  Add signup form component     (~80 lines, est. 35K tokens)  ✓ good size
  spi-abc.2  OAuth integration             (~150 lines, est. 65K tokens) ✓ good size
  spi-abc.3  Email verification service    (~300 lines, est. 120K tokens) ⚠ consider splitting
    → spi-abc.3a  Email sending logic      (~120 lines)
    → spi-abc.3b  Verification flow + tests (~180 lines)
  spi-abc.4  Profile setup wizard          (~100 lines, est. 45K tokens) ✓ good size
```

### 5. Capture winning prompts

When a task succeeds on the first try (no review rounds), capture
the full prompt context as a "golden run":

```json
{
  "bead_id": "spi-abc.1",
  "model": "claude-sonnet-4-6",
  "result": "first_pass_success",
  "context": {
    "system_prompt": "...",
    "spec_excerpt": "...",
    "focus_output": "...",
    "spire_yaml": "...",
    "claude_md": "..."
  },
  "context_tokens": 42000,
  "files_changed": ["src/auth/signup.tsx", "src/auth/signup.test.tsx"],
  "tags": ["react", "form", "auth", "small"]
}
```

Over time, this builds a corpus of "what works." When a new task
comes in, the refinery can look at similar successful runs and
suggest which context to include:

```
New task: "Add password reset form"
Similar successful tasks:
  - "Add signup form" (92% similar, first-pass success, 42K tokens)
  - "Add login form" (87% similar, first-pass success, 38K tokens)
Recommended context: spec + CLAUDE.md + src/auth/ directory listing
Estimated tokens: 40K
Estimated success probability: 89%
```

## Refinery architecture

### Container spec

```yaml
- name: refinery
  image: ghcr.io/awell-health/spire-refinery:latest
  env:
    - name: ANTHROPIC_API_KEY
      valueFrom:
        secretKeyRef:
          name: spire-credentials
          key: ANTHROPIC_API_KEY_HEAVY  # Opus token
    - name: SPIRE_EPIC_ID
      value: "<epic-bead-id>"
    - name: REFINERY_MAX_REVIEW_ROUNDS
      value: "3"
    - name: REFINERY_MODEL
      value: "claude-opus-4-6"
  resources:
    requests:
      cpu: 100m
      memory: 256Mi
```

### Refinery loop

```
while epic is open:
  1. Check for new branches (git fetch, check feat/<child-id>)
  2. For each new/updated branch:
     a. Run tests (from spire.yaml)
     b. If tests fail → send test output to worker, request fix
     c. If tests pass → run Opus review against spec
     d. If review approves → create/update PR
     e. If review requests changes → send to worker
  3. Check merge queue:
     a. For each approved PR in dependency order:
        - Attempt merge
        - Run tests on merged result
        - If tests pass → merge complete
        - If tests fail → roll back, notify
  4. Report epic progress to mayor
  5. Sleep 30s
```

### Refinery as an independent binary

```go
// cmd/spire-refinery/main.go
package main

// The refinery:
// - Watches git branches for an epic's children
// - Reviews diffs against specs using Opus
// - Creates and manages PRs
// - Manages the merge queue
// - Records metrics to the agent_runs table
// - Feeds back into task sizing and prompt optimization

func main() {
    epicID := os.Getenv("SPIRE_EPIC_ID")
    model := os.Getenv("REFINERY_MODEL") // default: claude-opus-4-6
    maxRounds := getenvInt("REFINERY_MAX_REVIEW_ROUNDS", 3)

    epic := loadEpic(epicID)
    children := loadChildren(epicID)

    for {
        for _, child := range children {
            branch := "feat/" + child.ID

            if !branchExists(branch) {
                continue
            }

            if !hasNewCommits(branch, child.lastReviewedCommit) {
                continue
            }

            // Run tests
            testResult := runTests(branch)
            if !testResult.Passed {
                sendToWorker(child, testResult.Output)
                recordRun(child, "test_failure", testResult)
                continue
            }

            // Opus review
            diff := gitDiff("main", branch)
            spec := loadSpec(epic)
            review := opusReview(model, spec, diff, testResult)

            recordRun(child, review.Verdict, review)

            switch review.Verdict {
            case "approve":
                createOrUpdatePR(child, branch, review.Summary)
                addToMergeQueue(child)
            case "request_changes":
                if child.ReviewRounds >= maxRounds {
                    escalateToHuman(child, review)
                } else {
                    sendToWorker(child, review.Issues)
                    child.ReviewRounds++
                }
            case "reject":
                reportToMayor(child, "rejected", review.Summary)
            }
        }

        processMergeQueue(epic)
        reportEpicProgress(epic)
        time.Sleep(30 * time.Second)
    }
}
```

## Data model: `agent_runs` table

New Dolt table for tracking all agent execution data:

```sql
CREATE TABLE agent_runs (
    id VARCHAR(32) PRIMARY KEY,
    bead_id VARCHAR(64) NOT NULL,
    epic_id VARCHAR(64),
    agent_name VARCHAR(128),
    model VARCHAR(64) NOT NULL,
    role ENUM('worker', 'refinery') NOT NULL,

    -- Execution metrics
    context_tokens_in INT,
    context_tokens_out INT,
    total_tokens INT,
    turns INT,
    duration_seconds INT,
    result ENUM('success', 'test_failure', 'review_rejected', 'timeout', 'error', 'stopped') NOT NULL,

    -- Review metrics (refinery role)
    review_rounds INT DEFAULT 0,
    refinery_verdict ENUM('approve', 'request_changes', 'reject'),

    -- Spec context
    spec_file VARCHAR(256),
    spec_size_tokens INT,
    focus_context_tokens INT,

    -- Code metrics
    files_changed INT,
    lines_added INT,
    lines_removed INT,
    tests_added INT,
    tests_passed BOOLEAN,

    -- Prompt capture (for learning)
    system_prompt_hash VARCHAR(64),    -- hash, not the full prompt (saves space)
    golden_run BOOLEAN DEFAULT FALSE,  -- first-pass success, worth studying

    -- Timestamps
    started_at DATETIME NOT NULL,
    completed_at DATETIME,

    -- Indexes
    INDEX idx_bead (bead_id),
    INDEX idx_epic (epic_id),
    INDEX idx_result (result),
    INDEX idx_golden (golden_run)
);

-- Prompt archive (separate table, stores full prompts for golden runs only)
CREATE TABLE golden_prompts (
    run_id VARCHAR(32) PRIMARY KEY,
    bead_id VARCHAR(64) NOT NULL,
    system_prompt TEXT,
    spec_excerpt TEXT,
    focus_context TEXT,
    tags JSON,
    context_tokens INT,
    FOREIGN KEY (run_id) REFERENCES agent_runs(id)
);
```

## Metrics and alerts

### Tracked automatically

| Metric | Alert threshold | Action |
|--------|----------------|--------|
| Context tokens used | >200K per task | Suggest splitting the bead |
| Review rounds | >3 per task | Escalate to human |
| Success rate (rolling 7d) | <70% | Review spec quality, adjust prompts |
| Time per task | >30min | Check for loops, consider model upgrade |
| API cost (daily) | >$50 (configurable) | Pause new assignments |
| Worker stuck (no commits) | >10min | Sidecar sends STOP |

### `spire metrics` command

```bash
spire metrics
# Today: 14 tasks completed, 89% first-pass success, avg 1.2 review rounds
# This week: 67 tasks, 84% success, $142 API cost
# Top specs by success rate:
#   auth-system.md          95% (12 tasks)
#   onboarding-flow.md      88% (8 tasks)
#   data-migration.md       62% (5 tasks) ← needs better spec

spire metrics --bead spi-abc
# Epic: 5 children, 3 done, 1 in review, 1 in progress
# Total tokens: 285K, Total cost: $8.40
# Avg review rounds: 1.4
# Estimated remaining: 2 tasks × avg $2.80 = ~$5.60

spire metrics --model
# Sonnet: 67 runs, avg 45K tokens, avg $1.80/task
# Opus (refinery): 52 reviews, avg 80K tokens, avg $4.20/review
# Total cost this week: $142 (workers: $95, refinery: $47)
```

## The feedback loop

```
                    ┌──────────────┐
                    │  Golden runs │
                    │  (what works)│
                    └──────┬───────┘
                           │
                           ▼
┌──────────┐    ┌──────────────────┐    ┌───────────┐
│  Specs   │───▶│  Task breakdown  │───▶│  Workers  │
│  (human) │    │  (informed by    │    │  (Sonnet) │
│          │    │   historical     │    │           │
│          │    │   success data)  │    │           │
└──────────┘    └──────────────────┘    └─────┬─────┘
                                              │
                                              ▼
                                        ┌───────────┐
                                        │  Refinery  │
                                        │  (Opus 1M) │
                                        │            │
                                        │  - Review  │
                                        │  - Record  │
                                        │  - Learn   │
                                        └─────┬─────┘
                                              │
                                   ┌──────────┼──────────┐
                                   ▼          ▼          ▼
                              Approve    Request     Metrics
                              → PR       changes     → agent_runs
                              → Merge    → Worker    → golden_prompts
                                         revises     → feed back into
                                                       task breakdown
```

The system gets better over time:
1. First month: you write detailed specs, agents succeed 70% on first pass
2. Third month: you know optimal task sizes, success rate hits 85%
3. Sixth month: `spire spec --break` auto-suggests task sizes based on
   your repo's historical data. Success rate 90%+.
4. Year one: the golden prompt corpus is rich enough that the refinery
   can suggest which context to include for new task types. You write
   shorter specs because the system fills in the patterns.

## Implementation priority

### Immediate (Phase 1 — get the pipeline working)

1. Agent worker image + entrypoint
2. Operator go.mod + pod creation
3. Basic refinery (test runner + Opus review)
4. `agent_runs` table in Dolt
5. End-to-end test on minikube

### Next (Phase 2 — PR lifecycle)

6. PR creation in refinery
7. Merge queue management
8. Review feedback loop (refinery → worker → refinery)
9. `spire watch` with epic aggregation
10. `spire steer` / `spire stop` via sidecar

### Then (Phase 3 — learning system)

11. Golden prompt capture
12. `spire metrics` command
13. Historical task sizing in `spire spec --break`
14. Similar-task lookup for context recommendation
15. Cost alerts and budget controls
