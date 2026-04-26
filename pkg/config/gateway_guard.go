package config

import (
	"errors"
	"fmt"
)

// ErrGatewayDirectMutation is the sentinel returned (wrapped) by every guard
// that rejects a direct Dolt mutation on a gateway-mode tower. Callers and
// tests match it via errors.Is so all the audited entry points — CLI
// push/pull/sync, daemon sync, dolt CLI helpers, bd wrappers,
// integration writers — share a single error identity even though the
// concrete error types differ (typed *GatewayModeError for top-level CLI,
// formatted wrap for library helpers).
//
// The audit grep marker for every guarded site is `gateway-mode:` in a
// trailing comment so future readers can enumerate every reviewed direct
// mutation path with `grep -rn "gateway-mode:" pkg/ cmd/`.
var ErrGatewayDirectMutation = errors.New("direct Dolt mutation rejected for gateway-mode tower")

// GatewayModeError is returned by RejectIfGateway when the resolved tower is
// in TowerModeGateway. It carries the tower name and gateway URL so the
// canonical error message can identify both the tower the operator selected
// and the endpoint mutations should route through.
//
// Callers may use errors.As to recover the typed error or errors.Is to match
// ErrGatewayDirectMutation; the message itself is stable:
// `tower <name> is gateway-mode; mutations route through <url>;
// direct Dolt sync is disabled`.
type GatewayModeError struct {
	TowerName  string
	GatewayURL string
}

func (e *GatewayModeError) Error() string {
	return fmt.Sprintf(
		"tower %s is gateway-mode; mutations route through %s; direct Dolt sync is disabled",
		e.TowerName, e.GatewayURL,
	)
}

// Is lets errors.Is(err, ErrGatewayDirectMutation) succeed for a typed
// *GatewayModeError so library helpers and CLI handlers can be matched
// uniformly. Both forms — typed (errors.As) and sentinel (errors.Is) —
// remain valid; this method only adds the sentinel match.
func (e *GatewayModeError) Is(target error) bool {
	return target == ErrGatewayDirectMutation
}

// IsGatewayMode is the cheap predicate used by steward/daemon iteration paths
// that already hold a TowerConfig and want to skip gateway-mode towers without
// re-resolving the active tower for each entry. CLI commands should use
// RejectIfGateway instead so SPIRE_TOWER / cfg.ActiveTower / CWD precedence
// matches the rest of the dispatch chain.
//
// Returns false for nil receivers so callers can pass the result of a
// loaded-from-disk slice element by address without a nil check.
func IsGatewayMode(tc *TowerConfig) bool {
	return tc != nil && tc.Mode == TowerModeGateway
}

// RejectIfGateway resolves the active tower via ResolveTowerConfig — the
// canonical resolver shared with store dispatch — and returns a
// *GatewayModeError when the tower is in TowerModeGateway. CLI commands that
// mutate Dolt directly (push, pull, sync) call this as their first
// executable line so a gateway-mode operator never reaches local Dolt or
// remote-mutation code paths.
//
// Resolver errors (no tower configured, ambiguous tower, IO failure) propagate
// verbatim — RejectIfGateway preserves the upstream "no tower" / ambiguity
// signal so command handlers don't need to care which case fires. Direct-mode
// towers return nil so handlers proceed normally.
func RejectIfGateway() error {
	tc, err := ResolveTowerConfig()
	if err != nil {
		return err
	}
	if IsGatewayMode(tc) {
		return &GatewayModeError{TowerName: tc.Name, GatewayURL: tc.URL}
	}
	return nil
}

// EnsureNotGateway is the explicit-cfg counterpart to RejectIfGateway: callers
// that already hold a *TowerConfig (steward iteration, library helpers reached
// from a context that resolved the tower upstream) pass it directly so the
// rejection does not re-resolve. op names the operation in the error string —
// pass the user-facing verb ("push", "pull", "dolt.CLIPush", "bd.DoltPush",
// "integration.ProcessWebhookQueue", etc.) so the audit trail names the
// rejected call.
//
// Returns nil for nil cfg (no active tower; treat as direct-mode for
// compatibility with library code that may run before a tower is loaded) and
// for direct-mode towers. Returns a wrapped ErrGatewayDirectMutation for
// gateway-mode towers; callers match via errors.Is, and the wrap embeds the
// op for diagnostic clarity.
//
// This helper exists alongside RejectIfGateway/IsGatewayMode rather than
// replacing them so the three idioms map to three call shapes:
//
//   - IsGatewayMode(cfg) — cheap predicate, no error formatting (steward
//     skip-loop; no resolver call wanted).
//   - RejectIfGateway() — top-level CLI guard, resolver-driven, returns the
//     canonical operator-facing message via *GatewayModeError.
//   - EnsureNotGateway(cfg, op) — library-helper guard with an explicit cfg
//     and an op string for the wrapped sentinel error.
func EnsureNotGateway(cfg *TowerConfig, op string) error {
	if cfg == nil {
		return nil
	}
	if cfg.Mode == TowerModeGateway {
		return fmt.Errorf("%w: operation %q must route through the gateway", ErrGatewayDirectMutation, op)
	}
	return nil
}

// EnsureNotGatewayResolved is the resolver-driven variant of EnsureNotGateway
// for library helpers that do not have a TowerConfig handy and want the
// sentinel-wrap error shape (rather than the *GatewayModeError shape that
// RejectIfGateway returns). It walks the canonical resolver — the same one
// store dispatch uses — and returns nil for direct-mode or a wrapped
// ErrGatewayDirectMutation for gateway-mode.
//
// Resolver errors (no tower configured, ambiguous tower) translate to
// "treat as direct-mode" so library helpers do not surface configuration
// problems through the gateway-rejection path. Top-level CLI commands should
// continue to use RejectIfGateway, which preserves resolver errors so the
// operator-facing message is correct.
func EnsureNotGatewayResolved(op string) error {
	tc, err := ResolveTowerConfig()
	if err != nil {
		return nil
	}
	return EnsureNotGateway(tc, op)
}
