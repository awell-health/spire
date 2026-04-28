package cleric

import (
	"context"
	"errors"
	"fmt"
)

// ErrGatewayUnimplemented is returned by GatewayClient.Execute when the
// gateway endpoint for a verb has not yet been implemented. The cleric
// runtime (spi-hhkozk) ships with the action catalog stubbed; the
// real gateway endpoints land separately under the action-catalog
// feature. cleric.execute treats this error as a soft failure and
// records it on the bead so the human reviewer can take over.
var ErrGatewayUnimplemented = errors.New("cleric: gateway action not yet implemented")

// ExecuteRequest is the payload GatewayClient.Execute sends to the
// gateway. Carries the recovery bead id (so the gateway can resolve the
// source bead via the caused-by dep) plus the proposal itself.
type ExecuteRequest struct {
	// RecoveryBeadID is the bead the cleric is recovering. Gateway
	// uses this to resolve the source bead via caused-by.
	RecoveryBeadID string

	// SourceBeadID is the bead the recovery action targets. Gateway
	// uses this to apply the action.
	SourceBeadID string

	// Proposal is the approved action.
	Proposal ProposedAction
}

// ExecuteResult is what GatewayClient.Execute returns on success.
type ExecuteResult struct {
	// Success indicates whether the gateway considers the action
	// applied. False with no error means "action ran but didn't meet
	// its success criteria" (gateway-defined per verb); true with no
	// error means done.
	Success bool

	// Message is a free-text outcome description — surfaces in the
	// recovery bead's metadata for the human reviewer.
	Message string
}

// GatewayClient is the seam through which cleric.execute calls the
// gateway action endpoints. The interface lets pkg/cleric stay free of
// the actual gateway client (HTTP, in-process call, whatever cmd/spire
// wires in) and lets tests inject fakes.
type GatewayClient interface {
	// Execute runs the proposed action. Returns ErrGatewayUnimplemented
	// when the verb has no gateway endpoint yet.
	Execute(ctx context.Context, req ExecuteRequest) (ExecuteResult, error)
}

// stubGateway is the default GatewayClient — returns
// ErrGatewayUnimplemented for every verb. cmd/spire replaces it with a
// real client at startup. Tests pass their own stub.
type stubGateway struct{}

// Execute always returns ErrGatewayUnimplemented.
func (stubGateway) Execute(_ context.Context, req ExecuteRequest) (ExecuteResult, error) {
	return ExecuteResult{
		Success: false,
		Message: fmt.Sprintf("gateway action %q not wired (cleric runtime v1)", req.Proposal.Verb),
	}, ErrGatewayUnimplemented
}

// DefaultGatewayClient is the package-level seam used by the action
// handlers when no client is injected via the action handler arguments.
// cmd/spire overrides this at startup. Tests overwrite it directly.
var DefaultGatewayClient GatewayClient = stubGateway{}
