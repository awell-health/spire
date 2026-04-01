package executor

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/steveyegge/beads"
)

// wizardPlanTask validates the approach for a standalone task and posts a
// focused implementation plan as a comment on the bead. The plan includes
// key files, approach, and context that the apprentice will use during
// the implement phase.
func (e *Executor) wizardPlanTask(bead Bead, pc PhaseConfig) error {
	// Check if a plan comment already exists (resume case).
	existingComments, _ := e.deps.GetComments(e.beadID)
	for _, c := range existingComments {
		if strings.HasPrefix(c.Text, "Implementation plan:") {
			e.log("task already has an implementation plan — skipping")
			return nil
		}
	}

	// Collect design context from linked design beads (discovered-from deps).
	designContext := e.collectDesignContext()

	// Build task context.
	var taskContext strings.Builder
	taskContext.WriteString(fmt.Sprintf("Bead: %s\nType: %s\nTitle: %s\nDescription: %s\n",
		bead.ID, bead.Type, bead.Title, bead.Description))
	for _, c := range existingComments {
		taskContext.WriteString(fmt.Sprintf("[%s]: %s\n", c.Author, c.Text))
	}

	prompt := fmt.Sprintf(`You are a Spire wizard planning a standalone task. Your job is to validate the approach and produce a focused implementation plan that an apprentice agent will follow.

Do NOT break this into subtasks unless the work genuinely requires parallel agents in isolated worktrees. Most tasks should be a single plan.

All context is provided below — do NOT read files or run commands.

## Task
%s

## Design Context
%s

## Planning rules

1. Identify the key files that need to be modified or created.
2. Describe the approach concisely — what changes, in what order, and why.
3. Call out edge cases, gotchas, or non-obvious constraints the implementer should know.
4. If the task is simple (e.g. a one-file bug fix), keep the plan short. Don't over-engineer.
5. If the task genuinely needs to be split into subtasks (rare for standalone tasks), say so and explain why.

## Output format

Produce a plan in this structure:

**Approach:** <1-2 sentence summary of the implementation strategy>

**Key files:**
- path/to/file.go — <what changes needed>

**Steps:**
1. <concrete implementation step>
2. <concrete implementation step>
...

**Edge cases / gotchas:**
- <anything non-obvious the implementer should watch for>

**Validation:**
- <how to verify the change works: commands, expected behavior>

Be precise and actionable. The apprentice implementing this will use your plan as their primary guide.
`, taskContext.String(), designContext)

	model := repoconfig.ResolveModel(pc.Model, e.repoModel())

	maxTurns := pc.GetMaxTurns()
	e.log("invoking Claude for task plan (max_turns=%d)", maxTurns)
	started := time.Now()
	out, err := e.deps.ClaudeRunner([]string{
		"--dangerously-skip-permissions",
		"-p", prompt,
		"--model", model,
		"--output-format", "text",
		"--max-turns", fmt.Sprintf("%d", maxTurns),
	}, e.state.RepoPath)
	e.recordAgentRun(e.agentName, e.beadID, "", model, "wizard", "plan", started, err)
	if err != nil {
		return fmt.Errorf("claude task plan: %w", err)
	}

	plan := strings.TrimSpace(string(out))
	if plan == "" {
		return fmt.Errorf("claude produced empty task plan")
	}

	// Post the plan as a comment on the bead.
	if commentErr := e.deps.AddComment(e.beadID, "Implementation plan:\n\n"+plan); commentErr != nil {
		return fmt.Errorf("post task plan comment: %w", commentErr)
	}
	e.log("posted implementation plan for %s", e.beadID)

	return nil
}

// collectDesignContext gathers context from linked design beads (discovered-from deps).
func (e *Executor) collectDesignContext() string {
	var designContext strings.Builder
	deps, _ := e.deps.GetDepsWithMeta(e.beadID)
	for _, dep := range deps {
		if dep.DependencyType != beads.DepDiscoveredFrom {
			continue
		}
		if dep.IssueType != "design" {
			continue
		}
		designContext.WriteString(fmt.Sprintf("--- Design bead %s: %s ---\n", dep.ID, dep.Title))
		if dep.Description != "" {
			designContext.WriteString(dep.Description + "\n")
		}
		comments, _ := e.deps.GetComments(dep.ID)
		for _, c := range comments {
			designContext.WriteString(fmt.Sprintf("[%s]: %s\n", c.Author, c.Text))
		}
		designContext.WriteString("\n")
	}
	return designContext.String()
}

// wizardPlanEpic reads the design bead(s) and invokes Claude to break the epic into subtasks.
func (e *Executor) wizardPlanEpic(bead Bead, pc PhaseConfig) error {
	// Collect design context from linked design beads (discovered-from deps)
	designContext := e.collectDesignContext()

	// Also include epic description and comments
	epicContext := fmt.Sprintf("Epic: %s\nTitle: %s\nDescription: %s\n", bead.ID, bead.Title, bead.Description)
	epicComments, _ := e.deps.GetComments(e.beadID)
	for _, c := range epicComments {
		epicContext += fmt.Sprintf("[%s]: %s\n", c.Author, c.Text)
	}

	// Check for existing children (resume case).
	// Filter out internal DAG beads (step, attempt, review-round) that are
	// created by ensureStepBeads/ensureAttemptBead before the plan phase runs.
	// Without this filter, planning is always skipped because those beads
	// make len(children) > 0 even when no real subtasks exist yet.
	allChildren, _ := e.deps.GetChildren(e.beadID)
	var children []Bead
	for _, c := range allChildren {
		if e.deps.IsAttemptBead(c) || e.deps.IsStepBead(c) || e.deps.IsReviewRoundBead(c) {
			continue
		}
		children = append(children, c)
	}
	if len(children) > 0 {
		e.log("epic already has %d children — enriching with change specs", len(children))
		return e.enrichSubtasksWithChangeSpecs(children, epicContext, designContext, pc)
	}

	prompt := fmt.Sprintf(`You are a Spire wizard planning an epic. Break the work into independent tasks that can be executed by parallel agents in isolated git worktrees.

All context is provided below — do NOT read files.

## Epic
%s

## Design Context
%s

## Planning rules

CRITICAL: Each task will be executed by a separate agent in its own git worktree. Agents CANNOT see each other's work. They run in parallel within a wave, then their branches are merged. Think carefully about:

1. SHARED FILES: If two tasks both need to modify the same file, they CANNOT be in the same wave. Put the foundational work (types, interfaces, shared utilities) in an earlier wave. Put consumers in a later wave.

2. DEPENDENCIES: If task B needs types/functions that task A creates, B depends on A. Use the "deps" field. Tasks with no deps run in the same wave (parallel).

3. NEGATIVE CONSTRAINTS: Each task must say what it does NOT do. If task A creates the query functions, task B's description must say "Do NOT create query functions — use the ones from task A." Without this, parallel agents will create conflicting implementations.

4. COMPLETE DESCRIPTIONS: Each task description must be self-contained. Include:
   - Exact file paths to create or modify
   - Function/component names and signatures
   - Expected behavior and edge cases
   - What files this task must NOT touch (handled by other tasks)
   Do NOT make agents read plan files or reference other tasks vaguely.

5. WAVE ORDERING: Wave 0 = shared foundations (types, queries, utilities). Wave 1 = features that consume wave 0. Wave 2 = integration, entry points, cross-cutting concerns.

## Output format

Output ONLY JSON objects, one per line, no other text. Each line:
{"title": "Short title", "description": "Complete task description with file paths, what to do, and what NOT to do", "deps": ["title-of-dependency"], "shared_files": ["path/to/shared/file.ts"], "do_not_touch": ["path/other-task-handles.ts"]}

- "shared_files": files this task creates/modifies that other tasks will consume (helps detect wave ordering issues)
- "do_not_touch": files this task must NOT modify (handled by another task)
- "deps": titles of tasks that must complete before this one starts
- Tasks with no deps run in parallel (same wave)
`, epicContext, designContext)

	model := repoconfig.ResolveModel(pc.Model, e.repoModel())

	maxTurns := pc.GetMaxTurns()
	e.log("invoking Claude for plan generation (max_turns=%d)", maxTurns)
	started := time.Now()
	out, err := e.deps.ClaudeRunner([]string{
		"--dangerously-skip-permissions",
		"-p", prompt,
		"--model", model,
		"--output-format", "text",
		"--max-turns", fmt.Sprintf("%d", maxTurns),
	}, e.state.RepoPath)
	e.recordAgentRun(e.agentName, e.beadID, "", model, "wizard", "plan", started, err)
	if err != nil {
		return fmt.Errorf("claude plan: %w", err)
	}

	// Parse subtasks from output
	type planTask struct {
		Title       string   `json:"title"`
		Description string   `json:"description"`
		Deps        []string `json:"deps"`
		SharedFiles []string `json:"shared_files"`
		DoNotTouch  []string `json:"do_not_touch"`
	}

	var tasks []planTask
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var t planTask
		if jsonErr := json.Unmarshal([]byte(line), &t); jsonErr == nil && t.Title != "" {
			tasks = append(tasks, t)
		}
	}

	if len(tasks) == 0 {
		e.log("Claude produced no parseable subtasks — posting raw output as comment")
		e.deps.AddComment(e.beadID, fmt.Sprintf("Wizard plan (raw):\n%s", string(out)))
		return fmt.Errorf("no subtasks parsed from plan output")
	}

	e.log("filing %d subtasks", len(tasks))

	// Create subtasks
	titleToID := make(map[string]string)
	for _, t := range tasks {
		desc := t.Description
		if len(t.SharedFiles) > 0 {
			desc += "\n\nShared files (other tasks depend on these): " + strings.Join(t.SharedFiles, ", ")
		}
		if len(t.DoNotTouch) > 0 {
			desc += "\n\nDo NOT touch (handled by other tasks): " + strings.Join(t.DoNotTouch, ", ")
		}
		id, createErr := e.deps.CreateBead(CreateOpts{
			Title:       t.Title,
			Description: desc,
			Priority:    bead.Priority,
			Type:        e.deps.ParseIssueType("task"),
			Parent:      e.beadID,
		})
		if createErr != nil {
			e.log("warning: create subtask %q: %s", t.Title, createErr)
			continue
		}
		titleToID[t.Title] = id
		e.log("  created %s: %s", id, t.Title)
	}

	// Add dependencies
	for _, t := range tasks {
		if len(t.Deps) == 0 {
			continue
		}
		taskID, ok := titleToID[t.Title]
		if !ok {
			continue
		}
		for _, depTitle := range t.Deps {
			depID, depOK := titleToID[depTitle]
			if !depOK {
				continue
			}
			e.deps.AddDep(taskID, depID)
		}
	}

	// Post plan summary as comment
	var planSummary strings.Builder
	planSummary.WriteString(fmt.Sprintf("Wizard plan: %d subtasks\n\n", len(tasks)))
	for _, t := range tasks {
		id := titleToID[t.Title]
		depStr := ""
		if len(t.Deps) > 0 {
			var depIDs []string
			for _, d := range t.Deps {
				if did, ok := titleToID[d]; ok {
					depIDs = append(depIDs, did)
				}
			}
			if len(depIDs) > 0 {
				depStr = " ← " + strings.Join(depIDs, ", ")
			}
		}
		planSummary.WriteString(fmt.Sprintf("- %s: %s%s\n", id, t.Title, depStr))
	}
	e.deps.AddComment(e.beadID, planSummary.String())

	return nil
}

// enrichSubtasksWithChangeSpecs invokes Claude per subtask to produce a change spec.
func (e *Executor) enrichSubtasksWithChangeSpecs(children []Bead, epicContext, designContext string, pc PhaseConfig) error {
	model := repoconfig.ResolveModel(pc.Model, e.repoModel())
	maxTurns := pc.GetMaxTurns()
	enriched := 0

	for _, child := range children {
		// Skip internal DAG beads.
		if e.deps.IsAttemptBead(child) || e.deps.IsStepBead(child) || e.deps.IsReviewRoundBead(child) {
			continue
		}
		// Skip already-enriched subtasks
		existingComments, _ := e.deps.GetComments(child.ID)
		alreadyEnriched := false
		for _, c := range existingComments {
			if strings.HasPrefix(c.Text, "Change spec:") {
				alreadyEnriched = true
				break
			}
		}
		if alreadyEnriched {
			e.log("subtask %s already has a change spec — skipping", child.ID)
			continue
		}

		// Build subtask context
		var subtaskContext strings.Builder
		subtaskContext.WriteString(fmt.Sprintf("Subtask ID: %s\nTitle: %s\nDescription: %s\n", child.ID, child.Title, child.Description))
		for _, c := range existingComments {
			subtaskContext.WriteString(fmt.Sprintf("[%s]: %s\n", c.Author, c.Text))
		}

		prompt := fmt.Sprintf(`You are a Spire wizard producing a change spec for a subtask. Your job is to analyze the subtask in context of the epic and design, then produce a precise, file-level specification of what code changes are needed.

All context is provided below — do NOT read files.

## Epic Context
%s

## Design Context
%s

## Subtask
%s

## Your output

Produce a change spec with this structure:

**Change spec: <subtask title>**

**Files to modify:**
- path/to/file.go — <what changes: add/modify function X, update struct Y, etc.>

**Files to create:**
- path/to/new_file.go — <purpose>

**Key functions/types:**
- <FunctionName>(args) — <what it does, signature if non-obvious>

**Rough diff shape:**
<Describe what the diff will look like: what's added, what's removed, what's changed. Be specific about function bodies, struct fields, interface methods. This is the single most important part — apprentices need to know exactly what to write.>

**Data flow (if non-obvious):**
<How data flows through the change — only include if the change involves multiple layers or non-obvious wiring.>

**Do NOT touch:**
<Files this subtask must not modify — handled by other tasks or unrelated.>

Be precise and concrete. The apprentice implementing this task will only see this spec — they cannot see other tasks or ask questions.
`, epicContext, designContext, subtaskContext.String())

		e.log("generating change spec for %s: %s", child.ID, child.Title)
		out, err := e.deps.ClaudeRunner([]string{
			"--dangerously-skip-permissions",
			"-p", prompt,
			"--model", model,
			"--output-format", "text",
			"--max-turns", fmt.Sprintf("%d", maxTurns),
		}, e.state.RepoPath)
		if err != nil {
			e.log("warning: change spec for %s: %s", child.ID, err)
			continue
		}

		spec := strings.TrimSpace(string(out))
		if spec == "" {
			e.log("warning: empty change spec for %s — skipping", child.ID)
			continue
		}

		if commentErr := e.deps.AddComment(child.ID, "Change spec:\n\n"+spec); commentErr != nil {
			e.log("warning: post change spec for %s: %s", child.ID, commentErr)
		} else {
			e.log("posted change spec for %s", child.ID)
			enriched++
		}
	}

	e.deps.AddComment(e.beadID, fmt.Sprintf("Wizard: enriched %d/%d subtasks with change specs.", enriched, len(children)))
	return nil
}

