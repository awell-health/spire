package store

import (
	"context"

	"github.com/steveyegge/beads"
)

// SetTestStorage installs a Storage backend for the active store singleton
// and returns a cleanup function that restores the previous state. This is a
// test-only helper exported so that downstream packages (e.g. pkg/board) can
// drive store-dependent code paths through the public API with a mock backend.
//
// Production code must not call this — only tests.
func SetTestStorage(s beads.Storage) func() {
	prev := activeStore
	prevCtx := storeCtx
	activeStore = s
	storeCtx = context.Background()
	return func() {
		activeStore = prev
		storeCtx = prevCtx
	}
}
