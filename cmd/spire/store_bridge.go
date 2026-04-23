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
	towerpkg "github.com/awell-health/spire/pkg/tower"
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

// storeGetBeadFunc is a test-replaceable function for storeGetBead.
var storeGetBeadFunc = storeGetBead

// storeGetActiveAttemptFunc is a test-replaceable function for storeGetActiveAttempt.
var storeGetActiveAttemptFunc = storeGetActiveAttempt

// storeRaiseCorruptedBeadAlertFunc is a test-replaceable function for storeRaiseCorruptedBeadAlert.
var storeRaiseCorruptedBeadAlertFunc = storeRaiseCorruptedBeadAlert

// storeGetDependentsWithMetaFunc is a test-replaceable function for storeGetDependentsWithMeta.
var storeGetDependentsWithMetaFunc = storeGetDependentsWithMeta

// storeStampAttemptInstanceFunc is a test-replaceable function for store.StampAttemptInstance.
var storeStampAttemptInstanceFunc = storeStampAttemptInstance

// storeIsOwnedByInstanceFunc is a test-replaceable function for store.IsOwnedByInstance.
var storeIsOwnedByInstanceFunc = storeIsOwnedByInstance

// storeGetAttemptInstanceFunc is a test-replaceable function for store.GetAttemptInstance.
var storeGetAttemptInstanceFunc = storeGetAttemptInstance

// storeCloseBeadFunc is a test-replaceable function for storeCloseBead.
var storeCloseBeadFunc = storeCloseBead

// storeDeleteBeadFunc is a test-replaceable function for storeDeleteBead.
var storeDeleteBeadFunc = storeDeleteBead

// storeCheckExistingAlertFunc checks whether an open corrupted-bead alert already exists.
// Checks both caused-by (current) and related (legacy) deps to find the link.
var storeCheckExistingAlertFunc = func(beadID string) bool {
	dependents, err := storeGetDependentsWithMeta(beadID)
	if err != nil {
		return false
	}
	for _, dep := range dependents {
		if dep.DependencyType != "caused-by" && dep.DependencyType != beads.DepRelated {
			continue
		}
		if dep.Status == beads.StatusClosed {
			continue
		}
		for _, l := range dep.Labels {
			if l == "alert:corrupted-bead" {
				return true
			}
		}
	}
	return false
}

// storeCreateAlertFunc creates the alert bead for a corrupted bead and links it via a caused-by dep.
var storeCreateAlertFunc = func(beadID, msg string) error {
	alertID, err := storeCreateBead(createOpts{
		Title:    msg,
		Priority: 0,
		Type:     beads.TypeTask,
		Labels:   []string{"alert:corrupted-bead"},
	})
	if err != nil {
		return err
	}
	if alertID != "" {
		if derr := storeAddDepTyped(alertID, beadID, "caused-by"); derr != nil {
			log.Printf("[store] warning: add caused-by dep %s→%s: %s", alertID, beadID, derr)
		}
	}
	return nil
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

func storeGetDependentsWithMeta(id string) ([]*beads.IssueWithDependencyMetadata, error) {
	return store.GetDependentsWithMeta(id)
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

func storeListRecoveryLearnings(filter store.RecoveryLookupFilter) ([]store.RecoveryLearning, error) {
	return store.ListClosedRecoveryBeads(filter)
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

func storeRemoveDep(issueID, dependsOnID string) error {
	return store.RemoveDep(issueID, dependsOnID)
}

func storeCloseBead(id string) error {
	return store.CloseBead(id)
}

func storeDeleteBead(id string) error {
	return store.DeleteBead(id)
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

func storeAddCommentAs(id, author, text string) error {
	return store.AddCommentAs(id, author, text)
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

func storeStampAttemptInstance(attemptID string, m store.InstanceMeta) error {
	return store.StampAttemptInstance(attemptID, m)
}

func storeIsOwnedByInstance(attemptID, instanceID string) (bool, error) {
	return store.IsOwnedByInstance(attemptID, instanceID)
}

func storeGetAttemptInstance(attemptID string) (*store.InstanceMeta, error) {
	return store.GetAttemptInstance(attemptID)
}

func storeCreateReviewBead(parentID, sageName string, round int) (string, error) {
	return store.CreateReviewBead(parentID, sageName, round)
}

func storeCreateStepBead(parentID, stepName string) (string, error) {
	return store.CreateStepBead(parentID, stepName)
}

func storeCloseReviewBead(reviewID, verdict, summary string, errorCount, warningCount, round int, findings []store.ReviewFinding) error {
	return store.CloseReviewBead(reviewID, verdict, summary, errorCount, warningCount, round, findings)
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

func storeHookStepBead(stepID string) error {
	return store.HookStepBead(stepID)
}

func storeUnhookStepBead(stepID string) error {
	return store.UnhookStepBead(stepID)
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

// --- Recovery BeadOps adapter ---

// storeBridgeOps implements recovery.BeadOps by delegating to store bridge
// functions. Used by resummon, reset, and other CLI paths that need to close
// recovery beads.
type storeBridgeOps struct{}

func (storeBridgeOps) GetDependentsWithMeta(id string) ([]*beads.IssueWithDependencyMetadata, error) {
	return storeGetDependentsWithMeta(id)
}

func (storeBridgeOps) AddComment(id, text string) error {
	return storeAddComment(id, text)
}

func (storeBridgeOps) CloseBead(id string) error {
	return storeCloseBead(id)
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

// --- Tower cluster attachment bridge ---

// towerAttachCluster records a ClusterAttachment on the active tower (or the
// tower named in opts.Tower). Thin wrapper around pkg/tower so cmd/spire
// paths call AttachCluster through the same bridge pattern used for store
// and formula operations.
func towerAttachCluster(opts towerpkg.AttachOptions) error {
	return towerpkg.AttachCluster(opts)
}

// --- Tower formula bridge (delegate to pkg/store with DB from active store) ---

func storeGetTowerFormula(name string) (string, error) {
	db, err := storeTowerDB()
	if err != nil {
		return "", err
	}
	return store.GetTowerFormula(db, name)
}

func storeListTowerFormulas() ([]store.TowerFormula, error) {
	db, err := storeTowerDB()
	if err != nil {
		return nil, err
	}
	return store.ListTowerFormulas(db)
}

func storePublishTowerFormula(name, content, desc, author string) error {
	db, err := storeTowerDB()
	if err != nil {
		return err
	}
	return store.PublishTowerFormula(db, name, content, desc, author)
}

func storeRemoveTowerFormula(name string) error {
	db, err := storeTowerDB()
	if err != nil {
		return err
	}
	return store.RemoveTowerFormula(db, name)
}

