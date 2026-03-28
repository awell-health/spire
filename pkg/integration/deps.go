// Package integration provides Linear integration: epic sync, webhook handling,
// OAuth2 connect flow, and the webhook HTTP server.
//
// It depends on store and config operations via callback functions that must be
// wired by the caller (cmd/spire/integration_bridge.go) before use.
package integration

import (
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// Bead is an alias for store.Bead used throughout the integration package.
type Bead = store.Bead

// --- Dependency callbacks (must be wired before calling any exported function) ---

// StoreListBeads lists beads matching a filter.
var StoreListBeads func(filter beads.IssueFilter) ([]Bead, error)

// StoreGetBead fetches a single bead by ID.
var StoreGetBead func(id string) (Bead, error)

// StoreCreateBead creates a new bead and returns its ID.
var StoreCreateBead func(opts store.CreateOpts) (string, error)

// StoreUpdateBead updates fields on an existing bead.
var StoreUpdateBead func(id string, updates map[string]interface{}) error

// StoreCloseBead closes a bead by ID.
var StoreCloseBead func(id string) error

// StoreAddLabel adds a label to a bead.
var StoreAddLabel func(id, label string) error

// StoreAddComment adds a comment to a bead.
var StoreAddComment func(id, text string) error

// StoreGetConfig reads a config value by key.
var StoreGetConfig func(key string) (string, error)

// StoreSetConfig writes a config value.
var StoreSetConfig func(key, val string) error

// StoreDeleteConfig deletes a config value.
var StoreDeleteConfig func(key string) error

// StoreGetActiveAttempt returns the active attempt bead for a parent.
var StoreGetActiveAttempt func(parentID string) (*Bead, error)

// StoreEnsure opens the store if not already open.
var StoreEnsure func() (beads.Storage, error)

// KeychainGet reads a secret from the system keychain.
var KeychainGet func(key string) (string, error)

// KeychainSet writes a secret to the system keychain.
var KeychainSet func(key, value string) error

// KeychainDelete removes a secret from the system keychain.
var KeychainDelete func(key string) error

// DoltSQL runs a SQL query against the Dolt server.
var DoltSQL func(query string, jsonOutput bool) (string, error)

// CmdSendFunc sends a spire message (delegates to cmdSend in cmd/spire).
var CmdSendFunc func(args []string) error

// HasLabel returns the value after the prefix if the bead has a label starting
// with prefix, or "" if not found. Delegates to store.HasLabel.
func HasLabel(b Bead, prefix string) string {
	return store.HasLabel(b, prefix)
}

// IssueTypePtr returns a pointer to an IssueType value.
func IssueTypePtr(t beads.IssueType) *beads.IssueType {
	return store.IssueTypePtr(t)
}

// StatusPtr returns a pointer to a Status value.
func StatusPtr(s beads.Status) *beads.Status {
	return store.StatusPtr(s)
}
