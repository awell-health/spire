package config

import "fmt"

// GatewayModeError is returned by RejectIfGateway when the resolved tower is
// in TowerModeGateway. It carries the tower name and gateway URL so the
// canonical error message can identify both the tower the operator selected
// and the endpoint mutations should route through.
//
// Callers may use errors.As to recover the typed error; the message itself is
// stable: `tower <name> is gateway-mode; mutations route through <url>;
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
