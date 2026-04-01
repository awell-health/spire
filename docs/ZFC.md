# ZFC (Zero Framework Cognition)

Spire is a thin, safe, deterministic orchestration shell around external AI reasoning.

The rule is simple:
- Local code may gather context, enforce structure, and execute decisions.
- Local code must not invent product reasoning, planning, ranking, or judgment that should come from the model.

## Allowed

These are ZFC-compliant:

- IO and plumbing: read and write files, list directories, parse or serialize JSON, persist state, watch events, index documents.
- Structural safety checks: schema validation, required-field checks, path safety, timeout enforcement, cancellation handling.
- Policy enforcement: approval gates, rate limits, budget caps, confidence thresholds.
- Mechanical transforms: parameter substitution, compilation, formatting, rendering model-provided data.
- State management: lifecycle tracking, progress monitoring, journaling, escalation execution.
- Typed error handling: use explicit error types and structured results instead of brittle message parsing.

## Forbidden

These are ZFC violations:

- Ranking or scoring alternatives with local heuristics or weights.
- Planning, dependency ordering, scheduling, retry strategy, or composition logic that should be model-driven.
- Semantic inference such as estimating scope, complexity, ownership, or "what should happen next."
- Heuristic classification or routing based on keyword tables, fallback trees, or domain-specific rules.
- Opinionated quality judgment beyond structural or policy checks.

## Required Pattern

Use this flow:

1. Gather raw context mechanically.
2. Ask the model for decisions, interpretation, composition, or prioritization.
3. Validate the result structurally and against policy.
4. Execute the accepted result mechanically.

## Practical Test

When adding logic, ask:

- Is this code only collecting context, enforcing safety, or executing a decision?
- Or is it silently deciding what matters, what is best, what is next, or how to rank options?

If it is making those decisions locally, it is probably a ZFC violation.

## Source

Concept adapted from Steve Yegge's Zero Framework Cognition essay.
