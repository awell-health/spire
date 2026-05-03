// Package clericexec wires the cleric.GatewayClient interface to the
// in-process action seams (pkg/summon, pkg/reset, pkg/close, pkg/store).
// This is the production adapter cmd/spire installs at startup so a
// cleric.execute call actually mutates state rather than returning
// ErrGatewayUnimplemented from the stub default.
//
// Why in-process and not HTTP: in local-native, the cleric runs as a
// child of the steward in the same binary as the gateway. Calling
// through HTTP would require the steward to discover the gateway's
// listen address and hold a long-lived HTTP client; the action seams
// are already exposed as Go packages, so we call them directly. The
// HTTP client (pkg/gatewayclient) is for laptops talking to a remote
// cluster gateway, a different topology.
//
// Spi-skfsia finding 2: before this adapter, cmd/spire never replaced
// cleric.DefaultGatewayClient and the stubGateway returned
// ErrGatewayUnimplemented for every verb. cleric.execute soft-recorded
// the unimplemented response, cleric.finish stamped approve+executed
// unconditionally, and the steward resumed the source bead even though
// nothing ran. Wiring this adapter closes that gap.
package clericexec

import (
	"context"
	"fmt"
	"strings"

	"github.com/awell-health/spire/pkg/cleric"
	pkgclose "github.com/awell-health/spire/pkg/close"
	"github.com/awell-health/spire/pkg/reset"
	"github.com/awell-health/spire/pkg/store"
	"github.com/awell-health/spire/pkg/summon"
)

// InProcClient is the production cleric.GatewayClient implementation
// for local-native. Each verb maps to one in-process action seam; the
// adapter is responsible for verb-arg validation and translating the
// underlying call's success/error into ExecuteResult.
//
// The zero value is usable: every field has a working default that
// dispatches to the production package functions. Tests construct a
// custom InProcClient with overridden fields to exercise the verb
// dispatch without booting the underlying packages.
type InProcClient struct {
	// ResummonFunc is summon.Run by default. The cleric's `resummon`
	// verb dispatches here. Required for the resummon verb to work.
	ResummonFunc func(beadID, dispatch string) (summon.Result, error)

	// DismissCloseFunc is pkg/close.RunLifecycle by default. Used by
	// the `dismiss` verb to close the source bead with a closed_reason
	// label.
	DismissCloseFunc func(id string) error

	// DismissResetFunc is reset.ResetBead{Hard:true} by default. Used
	// by `dismiss` to clean worktree+branch+graph state before close.
	DismissResetFunc func(ctx context.Context, beadID string) error

	// ResetHardFunc is reset.ResetBead{Hard:true} by default. Used by
	// the `reset --hard` verb. Distinct from DismissResetFunc so tests
	// can override one without the other.
	ResetHardFunc func(ctx context.Context, beadID string) error

	// AddLabelFunc is store.AddLabel by default. Used by the `dismiss`
	// verb to stamp closed_reason:dismissed on the bead before close.
	AddLabelFunc func(id, label string) error

	// UpdateBeadFunc is store.UpdateBead by default. Used by the
	// `update` and `update_status` verbs.
	UpdateBeadFunc func(id string, updates map[string]interface{}) error

	// AddCommentFunc is store.AddCommentReturning by default. Used by
	// the `comment-request-input` verb to write the cleric's question
	// onto the recovery bead.
	AddCommentFunc func(id, text string) (string, error)
}

// New returns an InProcClient with all seams wired to their production
// defaults. cmd/spire calls this at startup and assigns it to
// cleric.DefaultGatewayClient so cleric.execute dispatches against
// real package state.
func New() *InProcClient {
	return &InProcClient{
		ResummonFunc:     summon.Run,
		DismissCloseFunc: pkgclose.RunLifecycle,
		DismissResetFunc: func(ctx context.Context, beadID string) error {
			_, err := reset.ResetBead(ctx, reset.Opts{BeadID: beadID, Hard: true})
			return err
		},
		ResetHardFunc: func(ctx context.Context, beadID string) error {
			_, err := reset.ResetBead(ctx, reset.Opts{BeadID: beadID, Hard: true})
			return err
		},
		AddLabelFunc:    store.AddLabel,
		UpdateBeadFunc:  store.UpdateBead,
		AddCommentFunc:  store.AddCommentReturning,
	}
}

// Execute dispatches the proposal's verb to the matching in-process
// seam. Returns Success=true with a free-text Message describing what
// ran when the action lands; Success=false with a Message and a non-nil
// error when the action failed; ErrGatewayUnimplemented (wrapped) when
// the verb is not in the production catalog yet.
//
// The verb→seam mapping mirrors the v1 action manifest in pkg/cleric/
// actions.go and the gateway's handlers (pkg/gateway/actions.go). Any
// new verb that lands in the manifest must also land a case here, or
// the cleric will fall through to the unimplemented branch.
func (c *InProcClient) Execute(ctx context.Context, req cleric.ExecuteRequest) (cleric.ExecuteResult, error) {
	if c == nil {
		return cleric.ExecuteResult{}, fmt.Errorf("clericexec: nil InProcClient")
	}
	if req.SourceBeadID == "" {
		return cleric.ExecuteResult{Message: "missing source bead"}, fmt.Errorf("clericexec: source bead id is empty")
	}
	verb := strings.TrimSpace(req.Proposal.Verb)
	switch verb {
	case "resummon":
		return c.execResummon(req)
	case "dismiss":
		return c.execDismiss(ctx, req)
	case "reset --hard":
		return c.execResetHard(ctx, req)
	case "update":
		return c.execUpdate(req)
	case "update_status":
		return c.execUpdateStatus(req)
	case "comment-request-input":
		return c.execCommentRequest(req)
	}
	// Unknown verbs surface as ErrGatewayUnimplemented so cleric.execute
	// records the failure and the human reviewer can take over. This
	// also covers verbs (e.g. `reset --to <step>`) that are part of the
	// manifest but have not yet been wired through this client.
	return cleric.ExecuteResult{
			Message: fmt.Sprintf("verb %q is in the manifest but has no in-process adapter — extend pkg/clericexec to support it", verb),
		},
		fmt.Errorf("verb %q: %w", verb, cleric.ErrGatewayUnimplemented)
}

func (c *InProcClient) execResummon(req cleric.ExecuteRequest) (cleric.ExecuteResult, error) {
	if c.ResummonFunc == nil {
		return cleric.ExecuteResult{}, fmt.Errorf("clericexec: ResummonFunc is unwired")
	}
	res, err := c.ResummonFunc(req.SourceBeadID, "")
	if err != nil {
		return cleric.ExecuteResult{Message: err.Error()}, err
	}
	return cleric.ExecuteResult{Success: true, Message: "resummoned " + res.WizardName}, nil
}

func (c *InProcClient) execDismiss(ctx context.Context, req cleric.ExecuteRequest) (cleric.ExecuteResult, error) {
	if c.DismissResetFunc == nil || c.DismissCloseFunc == nil || c.AddLabelFunc == nil {
		return cleric.ExecuteResult{}, fmt.Errorf("clericexec: DismissResetFunc/DismissCloseFunc/AddLabelFunc unwired")
	}
	if err := c.DismissResetFunc(ctx, req.SourceBeadID); err != nil {
		return cleric.ExecuteResult{Message: "reset before dismiss: " + err.Error()}, err
	}
	// Stamp the dismissal reason BEFORE closing so it survives close
	// cascades. Non-fatal — close still runs.
	_ = c.AddLabelFunc(req.SourceBeadID, "closed_reason:dismissed")
	if err := c.DismissCloseFunc(req.SourceBeadID); err != nil {
		return cleric.ExecuteResult{Message: "close: " + err.Error()}, err
	}
	return cleric.ExecuteResult{Success: true, Message: "dismissed " + req.SourceBeadID}, nil
}

func (c *InProcClient) execResetHard(ctx context.Context, req cleric.ExecuteRequest) (cleric.ExecuteResult, error) {
	if c.ResetHardFunc == nil {
		return cleric.ExecuteResult{}, fmt.Errorf("clericexec: ResetHardFunc is unwired")
	}
	if err := c.ResetHardFunc(ctx, req.SourceBeadID); err != nil {
		return cleric.ExecuteResult{Message: "reset --hard: " + err.Error()}, err
	}
	return cleric.ExecuteResult{Success: true, Message: "reset --hard " + req.SourceBeadID}, nil
}

func (c *InProcClient) execUpdate(req cleric.ExecuteRequest) (cleric.ExecuteResult, error) {
	if c.UpdateBeadFunc == nil {
		return cleric.ExecuteResult{}, fmt.Errorf("clericexec: UpdateBeadFunc is unwired")
	}
	field := strings.TrimSpace(req.Proposal.Args["field"])
	value := strings.TrimSpace(req.Proposal.Args["value"])
	if field == "" || value == "" {
		return cleric.ExecuteResult{Message: "missing field or value arg"},
			fmt.Errorf("clericexec: update verb requires field+value args")
	}
	if err := c.UpdateBeadFunc(req.SourceBeadID, map[string]interface{}{field: value}); err != nil {
		return cleric.ExecuteResult{Message: "update " + field + ": " + err.Error()}, err
	}
	return cleric.ExecuteResult{Success: true, Message: "updated " + field}, nil
}

func (c *InProcClient) execUpdateStatus(req cleric.ExecuteRequest) (cleric.ExecuteResult, error) {
	if c.UpdateBeadFunc == nil {
		return cleric.ExecuteResult{}, fmt.Errorf("clericexec: UpdateBeadFunc is unwired")
	}
	to := strings.TrimSpace(req.Proposal.Args["to"])
	if to == "" {
		return cleric.ExecuteResult{Message: "missing to arg"},
			fmt.Errorf("clericexec: update_status verb requires to arg")
	}
	if err := c.UpdateBeadFunc(req.SourceBeadID, map[string]interface{}{"status": to}); err != nil {
		return cleric.ExecuteResult{Message: "update_status: " + err.Error()}, err
	}
	return cleric.ExecuteResult{Success: true, Message: "status → " + to}, nil
}

func (c *InProcClient) execCommentRequest(req cleric.ExecuteRequest) (cleric.ExecuteResult, error) {
	if c.AddCommentFunc == nil {
		return cleric.ExecuteResult{}, fmt.Errorf("clericexec: AddCommentFunc is unwired")
	}
	question := strings.TrimSpace(req.Proposal.Args["question"])
	if question == "" {
		return cleric.ExecuteResult{Message: "missing question arg"},
			fmt.Errorf("clericexec: comment-request-input verb requires question arg")
	}
	body := "[cleric-request-input] " + question
	if ctx := strings.TrimSpace(req.Proposal.Args["context"]); ctx != "" {
		body += "\n\nContext: " + ctx
	}
	// Write to the recovery bead, not the source — the cleric is asking
	// the human reviewer; the question lives next to the proposal.
	if _, err := c.AddCommentFunc(req.RecoveryBeadID, body); err != nil {
		return cleric.ExecuteResult{Message: "comment-request: " + err.Error()}, err
	}
	return cleric.ExecuteResult{Success: true, Message: "wrote question to recovery bead"}, nil
}
