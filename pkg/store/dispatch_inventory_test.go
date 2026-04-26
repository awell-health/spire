package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestDispatchInventory_Coverage walks every exported function in pkg/store
// (excluding _test.go files) and asserts each is either:
//
//   - listed in dispatchedAPIs as having a TowerModeGateway branch that
//     routes through gatewayclient or returns ErrGatewayUnsupported, or
//   - listed in nonDispatchableAPIs as a pure function with no Storage
//     access (helpers, conversion shims, type predicates).
//
// When a contributor adds a new public API, this test fails until the
// contributor either wires up the dispatch entry or explicitly adds the
// new name to nonDispatchableAPIs with a justification. The intent is the
// structural backstop the epic calls for: gateway-mode safety can't drift
// just because someone forgot to extend dispatch.go.
//
// Implementation: parse the package source via go/parser and collect the
// names of every top-level exported func. Compare against the union of
// the two registries below. Any unaccounted name causes a hard failure
// with the exact message: "add Foo to dispatch (or to nonDispatchableAPIs
// with reason)".
func TestDispatchInventory_Coverage(t *testing.T) {
	got, err := exportedFuncsInPackage(".")
	if err != nil {
		t.Fatalf("parse pkg/store sources: %v", err)
	}

	known := map[string]bool{}
	for _, n := range dispatchedAPIs {
		known[n] = true
	}
	for _, n := range nonDispatchableAPIs {
		known[n] = true
	}

	var missing []string
	for _, name := range got {
		if !known[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf(
			"public pkg/store APIs missing from dispatch coverage:\n  %s\n\n"+
				"add each to dispatchedAPIs (with a TowerModeGateway branch in the function body)\n"+
				"or to nonDispatchableAPIs (pure helper / no Storage access).",
			strings.Join(missing, "\n  "),
		)
	}

	// Also detect entries in the registries that no longer exist in source —
	// keeps the registries honest as code is removed/renamed.
	gotSet := map[string]bool{}
	for _, n := range got {
		gotSet[n] = true
	}
	var stale []string
	for _, name := range append(append([]string{}, dispatchedAPIs...), nonDispatchableAPIs...) {
		if !gotSet[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Fatalf(
			"dispatch registry references functions that no longer exist:\n  %s\n\n"+
				"remove these from dispatchedAPIs or nonDispatchableAPIs.",
			strings.Join(stale, "\n  "),
		)
	}
}

// exportedFuncsInPackage parses every non-test .go file in the given dir
// and returns the names of top-level exported functions. Methods on types
// are excluded — only `func Foo(...)` style declarations count, since
// methods are not part of the dispatch surface (callers reach them via
// concrete instances, not via the package-level API).
func exportedFuncsInPackage(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if fn.Recv != nil {
				continue // method, not a top-level func
			}
			if !ast.IsExported(fn.Name.Name) {
				continue
			}
			names = append(names, fn.Name.Name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// dispatchedAPIs lists every public pkg/store function that touches the
// Storage backend (directly or transitively) AND has an explicit
// TowerModeGateway branch in its body.
//
// The branch must either route through pkg/gatewayclient or return a
// wrapped ErrGatewayUnsupported BEFORE any code path reaches getStore()
// or local Dolt access. The fail-closed guard inside getStore() is the
// belt-and-suspenders backstop, but this list is the structural contract
// — everything here must visibly check isGatewayMode() up front.
//
// To audit: for each name below, grep `func <name>(` and confirm the
// first non-comment line matches the dispatch pattern. Adding a new
// entry without that branch will make TestDispatch_PanicStoreBackstop
// in dispatch_gateway_test.go panic.
var dispatchedAPIs = []string{
	// queries.go
	"GetBead",
	"GetIssue",
	"ListBeads",
	"ListBoardBeads",
	"GetDepsWithMeta",
	"GetConfig",
	"GetReadyWork",
	"GetBlockedIssues",
	"GetDependentsWithMeta",
	"GetComments",
	"GetChildren",
	"GetChildrenBatch",
	"GetChildrenBoardBatch",

	// mutations.go
	"CreateBead",
	"AddDep",
	"AddDepTyped",
	"RemoveDep",
	"CloseBead",
	"DeleteBead",
	"UpdateBead",
	"AddLabel",
	"RemoveLabel",
	"SetConfig",
	"DeleteConfig",
	"AddComment",
	"AddCommentReturning",
	"AddCommentAs",
	"AddCommentAsReturning",
	"CommitPending",

	// dispatch.go (messages and deps)
	"ListMessages",
	"SendMessage",
	"MarkMessageRead",
	"ListDeps",

	// recovery_learnings.go (sidecar SQL via getDB → getStore)
	"WriteRecoveryLearningAuto",
	"GetBeadLearningsAuto",
	"GetCrossBeadLearningsAuto",
	"GetLearningStatsAuto",
	"UpdateLearningOutcomeAuto",
	"GetPromotionSnapshotAuto",
	"DemotePromotedRowsAuto",
}

// nonDispatchableAPIs lists every public pkg/store function that does NOT
// need a TowerModeGateway branch. Three categories qualify:
//
//  1. Pure helpers / conversions (no I/O): IssueToBead, ParseStatus, etc.
//  2. Type predicates over already-fetched Beads: IsAttemptBead.
//  3. Functions that compose only already-dispatched APIs: HookStepBead
//     calls GetBead+UpdateBead, both dispatched, so it inherits the
//     gateway-mode behavior transitively.
//  4. Lifecycle-stamping helpers gated by activeStore != nil checks:
//     StampFiled, StampReady, etc. — under gateway mode activeStore is
//     never opened, so these become silent no-ops by construction.
//  5. Direct *sql.DB takers (recovery, trust, formulas, formula_routing):
//     callers must already have a *sql.DB, which is unreachable in
//     gateway mode (ActiveDB returns ok=false when activeStore is nil).
//  6. Test/scaffold setup: SetTestStorage, BeadsDirResolver wiring.
//
// Adding here requires brief justification in code review — the test
// reads this list at face value.
var nonDispatchableAPIs = []string{
	// store.go — pure conversions, no I/O.
	"Ensure",
	"OpenAt",
	"Open",
	"Reset",
	"Actor",
	"PopulateDependencies",
	"IssueToBead",
	"IssuesToBeads",
	"IssueToBoardBead",
	"IssuesToBoardBeads",
	"FindParentID",
	"StatusPtr",
	"IssueTypePtr",
	"ParseStatus",
	"ParseIssueType",
	"ParseIssueTypeOrTask",

	// bead.go — predicates over already-fetched Beads.
	"HasLabel",
	"ContainsLabel",

	// internal_types.go — pure predicates and a transitive composer.
	// MigrateInternalTypes calls only ListBeads + UpdateBead, both
	// dispatched, so it inherits the gateway-mode behavior.
	"IsWorkBead",
	"IsInternalBead",
	"IsInternalType",
	"MigrateInternalTypes",

	// beadtypes.go — composers over dispatched primitives, plus pure predicates.
	"GetActiveAttempt",
	"CreateAttemptBead",
	"CreateAttemptBeadAtomic",
	"CloseAttemptBead",
	"AttemptResult",
	"IsAttemptBead",
	"IsAttemptBoardBead",
	"CreateReviewBead",
	"CreateStepBead",
	"CloseReviewBead",
	"GetReviewBeads",
	"MostRecentReviewRound",
	"IsReviewRoundBead",
	"IsReviewRoundBoardBead",
	"ActivateStepBead",
	"CloseStepBead",
	"HookStepBead",
	"UnhookStepBead",
	"GetHookedSteps",
	"GetStepBeads",
	"GetActiveStep",
	"IsStepBead",
	"IsStepBoardBead",
	"IsFormulaTemplateBead",
	"IsFormulaTemplateBoardBead",
	"ReviewRoundNumber",
	"AttemptNumber",
	"ResetCycleNumber",
	"MaxRoundNumberFromBeads",
	"MaxRoundNumber",
	"MaxAttemptNumberFromBeads",
	"MaxAttemptNumber",
	"ParentResetCycle",
	"StepBeadPhaseName",

	// metadata.go — composers over dispatched primitives.
	"GetBeadMetadata",
	"SetBeadMetadata",
	"SetBeadMetadataMap",
	"AppendBeadMetadataList",
	"ListBeadsByMetadata",

	// instance_meta.go — composers over dispatched primitives.
	"StampAttemptInstance",
	"GetAttemptInstance",
	"IsOwnedByInstance",
	"UpdateAttemptHeartbeat",

	// schedule.go — composer over GetReadyWork (dispatched, fails closed).
	"GetSchedulableWork",

	// queries.go — composers / read-helpers.
	"ListClosedRecoveryBeads",
	"BugFilter",
	"GetCausedByDeps",
	"GetBugsCausedBy",

	// mutations.go — pure helper (string parsing).
	"PrefixFromID",

	// lifecycle.go — sidecar SQL gated on activeStore != nil; gateway-mode
	// never opens activeStore so these become no-ops.
	"ActiveDB",
	"StampFiled",
	"StampReady",
	"StampStarted",
	"StampClosed",
	"BackfillBeadLifecycle",

	// intent_transport.go — gated on ActiveDB ok; gateway-mode hits ok=false.
	"NextDispatchSeq",

	// recovery.go — direct *sql.DB takers; caller can't get a DB in gateway mode.
	"EnsureRecoveryAttemptsTable",
	"RecordRecoveryAttempt",
	"UpdateAttemptOutcome",
	"ListRecoveryAttempts",
	"GetLatestAttempt",
	"CountAttemptsByAction",

	// recovery_learning.go — direct *sql.DB takers.
	"CreateRecoveryLearning",
	"FindRecoveryLearnings",
	"FindMatchingLearning",
	"FindCrossBeadLearnings",

	// recovery_learnings.go — direct *sql.DB takers; *Auto wrappers are dispatched.
	"WriteRecoveryLearning",
	"GetBeadLearnings",
	"GetCrossBeadLearnings",
	"GetLearningStats",
	"UpdateLearningOutcome",
	"GetPromotionSnapshot",
	"DemotePromotedRows",

	// trust.go — direct *sql.DB takers.
	"TrustLevelName",
	"EnsureTrustTable",
	"GetTrustRecord",
	"UpsertTrustRecord",
	"ListTrustRecords",
	"RecordMergeOutcome",

	// formulas.go — direct *sql.DB takers.
	"GetTowerFormula",
	"ListTowerFormulas",
	"PublishTowerFormula",
	"RemoveTowerFormula",

	// formula_routing.go — direct *sql.DB takers.
	"EnsureFormulaExperimentsTable",
	"GetActiveExperiment",
	"CreateExperiment",
	"ConcludeExperiment",
	"PauseExperiment",
	"ListExperiments",
	"CompareVariants",

	// testing.go — test-only scaffolding.
	"SetTestStorage",

	// dispatch.go — gateway-client constructor; no Storage access. cmd/spire
	// callers (cmdClose's gateway-mode short-circuit) use this to bypass the
	// pkg/store dispatch layer entirely and talk to the gateway directly.
	"NewGatewayClientForTower",
}
