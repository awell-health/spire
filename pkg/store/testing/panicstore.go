// Package testing provides shared test doubles for pkg/store. It lives in a
// non-test file so callers across the repo (gateway-mode regression tests,
// integration harnesses) can import the helpers without _test.go scoping
// limitations.
package testing

import (
	"context"
	"time"

	"github.com/steveyegge/beads"
)

// PanicMessage is the panic value emitted by every PanicStore method.
// Tests assert on it to confirm dispatch.go never reached local Dolt
// when the active tower is gateway-mode.
const PanicMessage = "direct store access in gateway mode"

// PanicStore is a beads.Storage test double whose every method panics with
// PanicMessage. Used by gateway-mode regression tests to prove that under
// TowerModeGateway, the dispatch layer either routes through gatewayclient
// or fails closed before reaching local Dolt.
//
// PanicStore explicitly overrides the methods invoked from pkg/store's
// direct-mode code paths so the panic message clearly identifies which
// API leaked. Methods not listed here fall through to the embedded nil
// beads.Storage and trigger a runtime panic on call (with a less specific
// message); either signal fails the regression test.
//
// PanicStore is intentionally exported and lives outside _test.go so
// dispatch_gateway_test.go and any future cross-package gateway tests can
// install the same panicking double via store.SetTestStorage.
type PanicStore struct {
	beads.Storage
}

func panicAccess(op string) {
	panic(PanicMessage + ": " + op)
}

// --- Issues ---

func (PanicStore) CreateIssue(context.Context, *beads.Issue, string) error {
	panicAccess("CreateIssue")
	return nil
}

func (PanicStore) GetIssue(context.Context, string) (*beads.Issue, error) {
	panicAccess("GetIssue")
	return nil, nil
}

func (PanicStore) UpdateIssue(context.Context, string, map[string]interface{}, string) error {
	panicAccess("UpdateIssue")
	return nil
}

func (PanicStore) CloseIssue(context.Context, string, string, string, string) error {
	panicAccess("CloseIssue")
	return nil
}

func (PanicStore) DeleteIssue(context.Context, string) error {
	panicAccess("DeleteIssue")
	return nil
}

func (PanicStore) SearchIssues(context.Context, string, beads.IssueFilter) ([]*beads.Issue, error) {
	panicAccess("SearchIssues")
	return nil, nil
}

// --- Dependencies ---

func (PanicStore) AddDependency(context.Context, *beads.Dependency, string) error {
	panicAccess("AddDependency")
	return nil
}

func (PanicStore) RemoveDependency(context.Context, string, string, string) error {
	panicAccess("RemoveDependency")
	return nil
}

func (PanicStore) GetDependenciesWithMetadata(context.Context, string) ([]*beads.IssueWithDependencyMetadata, error) {
	panicAccess("GetDependenciesWithMetadata")
	return nil, nil
}

func (PanicStore) GetDependentsWithMetadata(context.Context, string) ([]*beads.IssueWithDependencyMetadata, error) {
	panicAccess("GetDependentsWithMetadata")
	return nil, nil
}

func (PanicStore) GetDependencyRecordsForIssues(context.Context, []string) (map[string][]*beads.Dependency, error) {
	panicAccess("GetDependencyRecordsForIssues")
	return nil, nil
}

// --- Labels ---

func (PanicStore) AddLabel(context.Context, string, string, string) error {
	panicAccess("AddLabel")
	return nil
}

func (PanicStore) RemoveLabel(context.Context, string, string, string) error {
	panicAccess("RemoveLabel")
	return nil
}

// --- Ready / Blocked ---

func (PanicStore) GetReadyWork(context.Context, beads.WorkFilter) ([]*beads.Issue, error) {
	panicAccess("GetReadyWork")
	return nil, nil
}

func (PanicStore) GetBlockedIssues(context.Context, beads.WorkFilter) ([]*beads.BlockedIssue, error) {
	panicAccess("GetBlockedIssues")
	return nil, nil
}

// --- Comments ---

func (PanicStore) AddIssueComment(context.Context, string, string, string) (*beads.Comment, error) {
	panicAccess("AddIssueComment")
	return nil, nil
}

func (PanicStore) GetIssueComments(context.Context, string) ([]*beads.Comment, error) {
	panicAccess("GetIssueComments")
	return nil, nil
}

func (PanicStore) ImportIssueComment(context.Context, string, string, string, time.Time) (*beads.Comment, error) {
	panicAccess("ImportIssueComment")
	return nil, nil
}

// --- Config / Metadata ---

func (PanicStore) SetConfig(context.Context, string, string) error {
	panicAccess("SetConfig")
	return nil
}

func (PanicStore) GetConfig(context.Context, string) (string, error) {
	panicAccess("GetConfig")
	return "", nil
}

func (PanicStore) DeleteConfig(context.Context, string) error {
	panicAccess("DeleteConfig")
	return nil
}

func (PanicStore) SetMetadata(context.Context, string, string) error {
	panicAccess("SetMetadata")
	return nil
}

func (PanicStore) GetMetadata(context.Context, string) (string, error) {
	panicAccess("GetMetadata")
	return "", nil
}

// --- Lifecycle ---

func (PanicStore) Close() error { return nil }
