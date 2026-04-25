package main

import (
	"sync"

	"github.com/awell-health/spire/pkg/config"
)

// authContextRegistry holds the AuthContext selected at summon time for each
// bead, keyed by bead ID. Spawn sites read it to inject the right env var
// into Claude subprocesses/pods. The registry lives in process memory: the
// 429 auto-promote handler may mutate Active in place, and subsequent spawns
// observe the swap automatically.
//
// This is a pragmatic local-state shim. Once the wizard run state is
// thread-throughout the spawn pipeline, this map can be retired in favor of
// passing the AuthContext directly through the call chain.
var (
	authContextMu sync.RWMutex
	authContexts  = map[string]*config.AuthContext{}
)

// setBeadAuthContext records the AuthContext selected for a bead at summon
// time. nil ctx removes any entry for the bead.
func setBeadAuthContext(beadID string, ctx *config.AuthContext) {
	authContextMu.Lock()
	defer authContextMu.Unlock()
	if ctx == nil {
		delete(authContexts, beadID)
		return
	}
	authContexts[beadID] = ctx
}

// getBeadAuthContext returns the AuthContext selected for a bead, or nil if
// nothing was set. Spawn sites that find nil should fall back to whatever
// env they would have used pre-feature (existing ANTHROPIC_API_KEY-from-env
// behavior) so summon paths that bypass selection don't break.
func getBeadAuthContext(beadID string) *config.AuthContext {
	authContextMu.RLock()
	defer authContextMu.RUnlock()
	return authContexts[beadID]
}

// clearBeadAuthContext removes the entry for a bead. Used by tests.
func clearBeadAuthContext(beadID string) {
	setBeadAuthContext(beadID, nil)
}

// ephemeralSuffix renders " (ephemeral)" when the context was synthesized
// from an inline -H header, else empty. Used to annotate the summon log.
func ephemeralSuffix(ctx *config.AuthContext) string {
	if ctx != nil && ctx.Ephemeral {
		return " (ephemeral)"
	}
	return ""
}

// authEnvForBead returns the env-var slice produced by AuthContext.InjectEnv
// for the bead's selected slot, suitable for SpawnConfig.AuthEnv. Returns
// (nil, "") when no context is registered for the bead — callers should
// treat that as "inherit whatever is in the parent process env".
func authEnvForBead(beadID string) (env []string, slot string) {
	ctx := getBeadAuthContext(beadID)
	if ctx == nil || ctx.Active == nil {
		return nil, ""
	}
	// Apply InjectEnv to a fresh empty base so the returned slice contains
	// only the managed Anthropic var. The spawn site merges it into the
	// child's full env.
	return ctx.InjectEnv(nil), ctx.SlotName()
}
