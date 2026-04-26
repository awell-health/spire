package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/gatewayclient"
	"github.com/steveyegge/beads"
)

// ErrNotFound is the store-level sentinel for "no such bead". Gateway mode
// translates gatewayclient.ErrNotFound into this so direct-mode and
// gateway-mode callers can branch on a single errors.Is check.
var ErrNotFound = errors.New("store: bead not found")

// ErrGatewayUnsupported is the sentinel returned by every public store API
// that has no gatewayclient equivalent yet. Wrapped with the operation name
// so error strings tell the operator which call must be routed (or added)
// rather than failing silently. CLI surfaces match it via errors.Is and
// surface a clear "this call must go through the gateway" message.
//
// The fail-closed guard inside getStore() also wraps this sentinel so any
// dispatch entry that slips past the gateway branch is caught before
// touching local Dolt.
var ErrGatewayUnsupported = errors.New("store: API not implemented for gateway-mode tower")

// gatewayUnsupportedErr returns ErrGatewayUnsupported wrapped with the
// public API name. Used by every dispatch entry that has no gateway client
// method and by the getStore() fail-closed guard.
func gatewayUnsupportedErr(op string) error {
	return fmt.Errorf("store API %q not implemented for gateway-mode tower: %w", op, ErrGatewayUnsupported)
}

// activeTowerFn resolves the current tower. Swapped in tests to inject a
// fake TowerConfig (gateway or direct) without touching real config files.
var activeTowerFn = resolveActiveTower

// newGatewayClientFn builds a gatewayclient.Client for a tower. Swapped in
// tests so httptest.Server URLs can stand in for a real gateway without
// reading the OS keychain.
var newGatewayClientFn = newGatewayClientReal

// activeTower returns the tower that bead/message/dep ops should route
// through. It reuses pkg/config.ResolveTowerConfig — the same resolution
// path resolveBeadsDir() relies on — so CLI contexts see a consistent view.
// Returning a nil *TowerConfig is treated as "direct mode" by callers.
func activeTower() (*config.TowerConfig, error) {
	return activeTowerFn()
}

// gatewayClient builds an HTTPS client for the tower's gateway. The bearer
// token is read from the OS keychain via config.GetTowerToken(t.Name); a
// missing token yields a clear error telling the user to re-attach.
func gatewayClient(t *config.TowerConfig) (*gatewayclient.Client, error) {
	return newGatewayClientFn(t)
}

// NewGatewayClientForTower is the public version of gatewayClient. cmd/spire
// callers (e.g. cmdClose's gateway-mode short-circuit) need a configured
// client without the rest of the dispatch layer; this exposes the same
// keychain-token + identity-header construction as the internal dispatchers
// so callers don't have to re-implement it.
func NewGatewayClientForTower(t *config.TowerConfig) (*gatewayclient.Client, error) {
	return newGatewayClientFn(t)
}

func resolveActiveTower() (*config.TowerConfig, error) {
	return config.ResolveTowerConfig()
}

func newGatewayClientReal(t *config.TowerConfig) (*gatewayclient.Client, error) {
	if t == nil {
		return nil, errors.New("store: no active tower for gateway client")
	}
	if t.URL == "" {
		return nil, fmt.Errorf("store: tower %q is gateway-mode but URL is empty — re-run 'spire tower attach-cluster'", t.Name)
	}
	token, err := config.GetTowerToken(t.Name)
	if err != nil {
		if errors.Is(err, config.ErrTokenNotFound) {
			return nil, fmt.Errorf("store: gateway token missing for tower %q — re-run 'spire tower attach-cluster --url=%s --token=<api-token>'", t.Name, t.URL)
		}
		return nil, fmt.Errorf("store: read gateway token for tower %q: %w", t.Name, err)
	}
	// Pass the local tower's archmage as the per-call identity so the
	// gateway records mutations under the calling desktop's archmage
	// instead of the cluster tower's static default. attach-cluster
	// initially seeds Archmage with the cluster's archmage, but the user
	// can override via `spire tower set --archmage-name/--archmage-email`
	// to record their own identity. Empty Name or Email suppresses the
	// headers — the gateway falls back to its tower default.
	id := gatewayclient.Identity{
		Name:  t.Archmage.Name,
		Email: t.Archmage.Email,
	}
	return gatewayclient.NewClientWithIdentity(t.URL, token, id), nil
}

// isGatewayMode reports whether ops should route through a gateway client.
// Resolution errors (no tower configured, config unreadable) fall back to
// direct mode so existing local flows keep working.
func isGatewayMode() (*config.TowerConfig, bool) {
	t, err := activeTower()
	if err != nil || t == nil {
		return nil, false
	}
	return t, t.IsGateway()
}

// dispatchContext returns a bounded context for a gateway round-trip.
// Callers that already have a context should wrap their own instead;
// this helper exists for the pkg/store public API which is context-free.
func dispatchContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// translateGatewayError maps gatewayclient sentinel errors onto pkg/store
// sentinels so direct-mode and gateway-mode callers can't tell the
// difference when matching with errors.Is.
func translateGatewayError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, gatewayclient.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// beadFromRecord converts a gatewayclient.BeadRecord to the pkg/store
// lightweight Bead shape. Used by every read path that returns Bead from
// gateway mode (GetBead, ListBeads, ListMessages).
func beadFromRecord(r gatewayclient.BeadRecord) Bead {
	return Bead{
		ID:          r.ID,
		Title:       r.Title,
		Description: r.Description,
		Status:      r.Status,
		Priority:    r.Priority,
		Type:        r.Type,
		Labels:      r.Labels,
		Parent:      r.Parent,
		UpdatedAt:   r.UpdatedAt,
		Metadata:    r.Metadata,
	}
}

func beadsFromRecords(rs []gatewayclient.BeadRecord) []Bead {
	out := make([]Bead, len(rs))
	for i, r := range rs {
		out[i] = beadFromRecord(r)
	}
	return out
}

// --- Gateway-mode helpers for each dispatched operation ---

func getBeadGateway(t *config.TowerConfig, id string) (Bead, error) {
	c, err := gatewayClient(t)
	if err != nil {
		return Bead{}, err
	}
	ctx, cancel := dispatchContext()
	defer cancel()
	rec, err := c.GetBead(ctx, id)
	if err != nil {
		return Bead{}, translateGatewayError(err)
	}
	return beadFromRecord(rec), nil
}

func listBeadsGateway(t *config.TowerConfig, filter beads.IssueFilter) ([]Bead, error) {
	c, err := gatewayClient(t)
	if err != nil {
		return nil, err
	}
	gf := gatewayclient.ListBeadsFilter{}
	if filter.Status != nil {
		gf.Status = string(*filter.Status)
	}
	if filter.IssueType != nil {
		gf.Type = string(*filter.IssueType)
	}
	if filter.IDPrefix != "" {
		// pkg/store callers pass "spi-" style prefixes; strip the trailing
		// "-" so the gateway query param matches the repo key (e.g. "spi").
		gf.Prefix = strings.TrimSuffix(filter.IDPrefix, "-")
	}
	if len(filter.Labels) > 0 {
		gf.Label = strings.Join(filter.Labels, ",")
	}
	ctx, cancel := dispatchContext()
	defer cancel()
	recs, err := c.ListBeads(ctx, gf)
	if err != nil {
		return nil, translateGatewayError(err)
	}
	return beadsFromRecords(recs), nil
}

func createBeadGateway(t *config.TowerConfig, opts CreateOpts) (string, error) {
	c, err := gatewayClient(t)
	if err != nil {
		return "", err
	}
	in := gatewayclient.CreateBeadInput{
		Title:       opts.Title,
		Type:        string(opts.Type),
		Priority:    opts.Priority,
		Description: opts.Description,
		Labels:      opts.Labels,
		Parent:      opts.Parent,
		Prefix:      opts.Prefix,
	}
	ctx, cancel := dispatchContext()
	defer cancel()
	id, err := c.CreateBead(ctx, in)
	if err != nil {
		return "", translateGatewayError(err)
	}
	return id, nil
}

func updateBeadGateway(t *config.TowerConfig, id string, updates map[string]interface{}) error {
	c, err := gatewayClient(t)
	if err != nil {
		return err
	}
	ctx, cancel := dispatchContext()
	defer cancel()
	return translateGatewayError(c.UpdateBead(ctx, id, updates))
}

func getBlockedIssuesGateway(t *config.TowerConfig, _ beads.WorkFilter) ([]BoardBead, error) {
	c, err := gatewayClient(t)
	if err != nil {
		return nil, err
	}
	ctx, cancel := dispatchContext()
	defer cancel()
	resp, err := c.GetBlockedIssues(ctx)
	if err != nil {
		return nil, translateGatewayError(err)
	}
	// The gateway endpoint returns only IDs today; emit ID-only BoardBeads.
	// Callers that need full hydration issue follow-up GetBead calls.
	out := make([]BoardBead, 0, len(resp.IDs))
	for _, id := range resp.IDs {
		out = append(out, BoardBead{ID: id})
	}
	return out, nil
}

// --- New dispatched functions: messages and deps ---

// SendMessageOpts captures the parameters for SendMessage. Matches the
// fields the gateway's POST /api/v1/messages endpoint accepts and the
// labels the direct-mode flow stamps onto a message bead.
type SendMessageOpts struct {
	To       string
	From     string
	Message  string
	Ref      string // bead ID reference; emitted as ref:<id> label in direct mode
	Thread   string // parent thread ID (creates parent-child dep in direct mode)
	Priority int
}

// ListMessages returns open message beads addressed to `to`. Empty `to`
// returns every open message. Gateway mode calls GET /api/v1/messages;
// direct mode queries bead storage filtered by msg/to:<name> labels.
func ListMessages(to string) ([]Bead, error) {
	if t, ok := isGatewayMode(); ok {
		return listMessagesGateway(t, to)
	}
	return listMessagesDirect(to)
}

func listMessagesDirect(to string) ([]Bead, error) {
	labels := []string{"msg"}
	if to != "" {
		labels = append(labels, "to:"+to)
	}
	open := beads.StatusOpen
	return listBeadsDirect(beads.IssueFilter{
		Labels: labels,
		Status: &open,
	})
}

func listMessagesGateway(t *config.TowerConfig, to string) ([]Bead, error) {
	c, err := gatewayClient(t)
	if err != nil {
		return nil, err
	}
	ctx, cancel := dispatchContext()
	defer cancel()
	recs, err := c.ListMessages(ctx, to)
	if err != nil {
		return nil, translateGatewayError(err)
	}
	return beadsFromRecords(recs), nil
}

// SendMessage creates a message bead and returns its ID. In direct mode
// the bead is stamped with msg/to:/from: labels (and ref:/parent if set).
// In gateway mode the call hits POST /api/v1/messages and the gateway
// applies the labels server-side.
func SendMessage(opts SendMessageOpts) (string, error) {
	if t, ok := isGatewayMode(); ok {
		return sendMessageGateway(t, opts)
	}
	return sendMessageDirect(opts)
}

func sendMessageDirect(opts SendMessageOpts) (string, error) {
	labels := []string{"msg", "to:" + opts.To}
	if opts.From != "" {
		labels = append(labels, "from:"+opts.From)
	}
	if opts.Ref != "" {
		labels = append(labels, "ref:"+opts.Ref)
	}
	return createBeadDirect(CreateOpts{
		Title:    opts.Message,
		Priority: opts.Priority,
		Type:     beads.IssueType("message"),
		Prefix:   "spi",
		Labels:   labels,
		Parent:   opts.Thread,
	})
}

func sendMessageGateway(t *config.TowerConfig, opts SendMessageOpts) (string, error) {
	c, err := gatewayClient(t)
	if err != nil {
		return "", err
	}
	in := gatewayclient.SendMessageInput{
		To:       opts.To,
		Message:  opts.Message,
		From:     opts.From,
		Ref:      opts.Ref,
		Thread:   opts.Thread,
		Priority: opts.Priority,
	}
	ctx, cancel := dispatchContext()
	defer cancel()
	id, err := c.SendMessage(ctx, in)
	if err != nil {
		return "", translateGatewayError(err)
	}
	return id, nil
}

// MarkMessageRead marks a message bead as read. In direct mode the bead is
// closed (matching the existing `spire read` semantics); in gateway mode
// the call hits POST /api/v1/messages/{id}/read which the gateway
// implements as the same bead-close on its end.
func MarkMessageRead(id string) error {
	if t, ok := isGatewayMode(); ok {
		return markMessageReadGateway(t, id)
	}
	return markMessageReadDirect(id)
}

func markMessageReadDirect(id string) error {
	return CloseBead(id)
}

func markMessageReadGateway(t *config.TowerConfig, id string) error {
	c, err := gatewayClient(t)
	if err != nil {
		return err
	}
	ctx, cancel := dispatchContext()
	defer cancel()
	return translateGatewayError(c.MarkMessageRead(ctx, id))
}

// ListDeps returns every dependency edge where `id` is the dependent side.
// Direct mode derives edges from GetDepsWithMeta; gateway mode calls
// GET /api/v1/beads/{id}/deps. Both return the same BoardDep shape.
func ListDeps(id string) ([]BoardDep, error) {
	if t, ok := isGatewayMode(); ok {
		return listDepsGateway(t, id)
	}
	return listDepsDirect(id)
}

func listDepsDirect(id string) ([]BoardDep, error) {
	deps, err := GetDepsWithMeta(id)
	if err != nil {
		return nil, err
	}
	out := make([]BoardDep, 0, len(deps))
	for _, d := range deps {
		out = append(out, BoardDep{
			IssueID:     id,
			DependsOnID: d.ID,
			Type:        string(d.DependencyType),
		})
	}
	return out, nil
}

func listDepsGateway(t *config.TowerConfig, id string) ([]BoardDep, error) {
	c, err := gatewayClient(t)
	if err != nil {
		return nil, err
	}
	ctx, cancel := dispatchContext()
	defer cancel()
	recs, err := c.ListDeps(ctx, id)
	if err != nil {
		return nil, translateGatewayError(err)
	}
	out := make([]BoardDep, 0, len(recs))
	for _, r := range recs {
		out = append(out, BoardDep{
			IssueID:     r.IssueID,
			DependsOnID: r.DependsOnID,
			Type:        r.Type,
		})
	}
	return out, nil
}
