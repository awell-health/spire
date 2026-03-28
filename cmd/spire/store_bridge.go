// store_bridge.go provides backward-compatible wrappers for cmd/spire callers.
// Most functions delegate to pkg/store. A few functions that depend on test-replaceable
// vars are re-implemented here so that tests in cmd/spire can swap behavior without
// affecting pkg/store internals.
package main

import (
	"fmt"
	"log"

	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// --- Type aliases ---

type Bead = store.Bead
type BoardBead = store.BoardBead
type BoardDep = store.BoardDep
type createOpts = store.CreateOpts

// --- Test-replaceable vars ---
// These live in cmd/spire (not pkg/store) because the tests that swap them are here.

// storeGetChildrenFunc is a test-replaceable function for storeGetChildren.
var storeGetChildrenFunc = storeGetChildren

// storeGetActiveAttemptFunc is a test-replaceable function for storeGetActiveAttempt.
var storeGetActiveAttemptFunc = storeGetActiveAttempt

// storeRaiseCorruptedBeadAlertFunc is a test-replaceable function for storeRaiseCorruptedBeadAlert.
var storeRaiseCorruptedBeadAlertFunc = storeRaiseCorruptedBeadAlert

// storeCheckExistingAlertFunc checks whether an open corrupted-bead alert already exists.
var storeCheckExistingAlertFunc = func(beadID string) bool {
	existing, err := storeListBeads(beads.IssueFilter{
		Labels: []string{"alert:corrupted-bead", "ref:" + beadID},
	})
	return err == nil && len(existing) > 0
}

// storeCreateAlertFunc creates the alert bead for a corrupted bead.
var storeCreateAlertFunc = func(beadID, msg string) error {
	_, err := storeCreateBead(createOpts{
		Title:    msg,
		Priority: 0,
		Type:     beads.TypeTask,
		Labels:   []string{"alert:corrupted-bead", "ref:" + beadID},
	})
	return err
}

// init wires up cross-package callbacks:
//   - pkg/store.BeadsDirResolver  ← config.ResolveBeadsDir
//   - pkg/config.DoltDataDirFunc  ← doltDataDir (from doltserver.go)
//   - pkg/config.StoreConfigGetterFunc ← storeGetConfig (from this bridge)
func init() {
	store.BeadsDirResolver = resolveBeadsDir
	config.DoltDataDirFunc = doltDataDir
	config.StoreConfigGetterFunc = storeGetConfig
}

// --- Store lifecycle ---

func ensureStore() (beads.Storage, error) {
	return store.Ensure(resolveBeadsDir())
}

func openStoreAt(beadsDir string) (beads.Storage, error) {
	return store.OpenAt(beadsDir)
}

func resetStore() {
	store.Reset()
}

// --- Queries (delegate to pkg/store) ---

func storeGetBead(id string) (Bead, error) {
	return store.GetBead(id)
}

func storeListBeads(filter beads.IssueFilter) ([]Bead, error) {
	return store.ListBeads(filter)
}

func storeListBoardBeads(filter beads.IssueFilter) ([]BoardBead, error) {
	return store.ListBoardBeads(filter)
}

func storeGetDepsWithMeta(id string) ([]*beads.IssueWithDependencyMetadata, error) {
	return store.GetDepsWithMeta(id)
}

func storeGetConfig(key string) (string, error) {
	return store.GetConfig(key)
}

func storeGetReadyWork(filter beads.WorkFilter) ([]Bead, error) {
	return store.GetReadyWork(filter)
}

func storeGetBlockedIssues(filter beads.WorkFilter) ([]BoardBead, error) {
	return store.GetBlockedIssues(filter)
}

func storeGetComments(id string) ([]*beads.Comment, error) {
	return store.GetComments(id)
}

func storeGetChildren(parentID string) ([]Bead, error) {
	return store.GetChildren(parentID)
}

// --- Mutations (delegate to pkg/store) ---

func storeCreateBead(opts createOpts) (string, error) {
	return store.CreateBead(opts)
}

func storeAddDep(issueID, dependsOnID string) error {
	return store.AddDep(issueID, dependsOnID)
}

func storeAddDepTyped(issueID, dependsOnID, depType string) error {
	return store.AddDepTyped(issueID, dependsOnID, depType)
}

func storeCloseBead(id string) error {
	return store.CloseBead(id)
}

func storeUpdateBead(id string, updates map[string]interface{}) error {
	return store.UpdateBead(id, updates)
}

func storeAddLabel(id, label string) error {
	return store.AddLabel(id, label)
}

func storeRemoveLabel(id, label string) error {
	return store.RemoveLabel(id, label)
}

func storeSetConfig(key, val string) error {
	return store.SetConfig(key, val)
}

func storeDeleteConfig(key string) error {
	return store.DeleteConfig(key)
}

func storeAddComment(id, text string) error {
	return store.AddComment(id, text)
}

func storeCommitPending(message string) error {
	return store.CommitPending(message)
}

// --- Bead type helpers (delegate to pkg/store) ---

func storeGetActiveAttempt(parentID string) (*Bead, error) {
	return store.GetActiveAttempt(parentID)
}

func storeCreateAttemptBead(parentID, agentName, model, branch string) (string, error) {
	return store.CreateAttemptBead(parentID, agentName, model, branch)
}

func storeCreateAttemptBeadAtomic(parentID, agentName, model, branch string) (string, error) {
	return store.CreateAttemptBeadAtomic(parentID, agentName, model, branch)
}

func storeCloseAttemptBead(attemptID, result string) error {
	return store.CloseAttemptBead(attemptID, result)
}

func storeCreateReviewBead(parentID, sageName string, round int) (string, error) {
	return store.CreateReviewBead(parentID, sageName, round)
}

func storeCreateStepBead(parentID, stepName string) (string, error) {
	return store.CreateStepBead(parentID, stepName)
}

func storeCloseReviewBead(reviewID, verdict, summary string) error {
	return store.CloseReviewBead(reviewID, verdict, summary)
}

// storeGetReviewBeads uses the test-replaceable storeGetChildrenFunc so tests
// can inject fake children without needing a real store.
func storeGetReviewBeads(parentID string) ([]Bead, error) {
	children, err := storeGetChildrenFunc(parentID)
	if err != nil {
		return nil, err
	}
	var reviews []Bead
	for _, child := range children {
		if isReviewRoundBead(child) {
			reviews = append(reviews, child)
		}
	}
	// Sort by round number.
	for i := 0; i < len(reviews); i++ {
		for j := i + 1; j < len(reviews); j++ {
			ri := reviewRoundNumber(reviews[i])
			rj := reviewRoundNumber(reviews[j])
			if rj < ri {
				reviews[i], reviews[j] = reviews[j], reviews[i]
			}
		}
	}
	return reviews, nil
}

func storeActivateStepBead(stepID string) error {
	return store.ActivateStepBead(stepID)
}

func storeCloseStepBead(stepID string) error {
	return store.CloseStepBead(stepID)
}

func storeGetStepBeads(parentID string) ([]Bead, error) {
	return store.GetStepBeads(parentID)
}

func storeGetActiveStep(parentID string) (*Bead, error) {
	return store.GetActiveStep(parentID)
}

// storeRaiseCorruptedBeadAlert uses bridge-level test-replaceable vars so tests
// can verify dedup and creation behavior.
func storeRaiseCorruptedBeadAlert(beadID string, violation error) {
	if storeCheckExistingAlertFunc(beadID) {
		log.Printf("[store] alert already exists for corrupted bead %s, skipping duplicate", beadID)
		return
	}
	msg := fmt.Sprintf("corrupted bead %s: %v", beadID, violation)
	if err := storeCreateAlertFunc(beadID, msg); err != nil {
		log.Printf("[store] failed to raise alert for corrupted bead %s: %v", beadID, err)
	}
}

// --- Predicates ---

func isAttemptBead(b Bead) bool {
	return store.IsAttemptBead(b)
}

func isAttemptBoardBead(b BoardBead) bool {
	return store.IsAttemptBoardBead(b)
}

func isReviewRoundBead(b Bead) bool {
	return store.IsReviewRoundBead(b)
}

func isReviewRoundBoardBead(b BoardBead) bool {
	return store.IsReviewRoundBoardBead(b)
}

func isStepBead(b Bead) bool {
	return store.IsStepBead(b)
}

func isStepBoardBead(b BoardBead) bool {
	return store.IsStepBoardBead(b)
}

// --- Conversion / filter helpers ---
// Dead wrappers removed: issueToBead, issuesToBeads, issueToBoardBead,
// issuesToBoardBeads, findParentID — no callers remain in cmd/spire.

func statusPtr(s beads.Status) *beads.Status {
	return store.StatusPtr(s)
}

func issueTypePtr(t beads.IssueType) *beads.IssueType {
	return store.IssueTypePtr(t)
}

func parseStatus(s string) beads.Status {
	return store.ParseStatus(s)
}

func parseIssueType(s string) beads.IssueType {
	return store.ParseIssueType(s)
}

// storeActor removed — no callers in cmd/spire.

// --- Label helpers ---

func hasLabel(b Bead, prefix string) string {
	return store.HasLabel(b, prefix)
}

func containsLabel(b Bead, label string) bool {
	return store.ContainsLabel(b, label)
}

// --- Phase helpers ---

func reviewRoundNumber(b Bead) int {
	return store.ReviewRoundNumber(b)
}

func stepBeadPhaseName(b Bead) string {
	return store.StepBeadPhaseName(b)
}
