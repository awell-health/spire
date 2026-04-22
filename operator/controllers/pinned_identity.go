// Pinned identity bead lifecycle for WizardGuild.Cache (spi-2bgsm).
//
// A pinned identity bead is the stable, non-actionable bead that
// represents a cluster resource (here, a WizardGuild's repo cache) in
// the bead graph. Wisp recovery beads filed when the resource fails
// (spi-htay5, separate task) point at this pinned bead via a
// `caused-by` dependency. The cleric (spi-3w7d8) walks that edge to
// diagnose; the operator's only job here is to maintain the pinned
// bead's lifecycle alongside the WizardGuild CR.
//
// Lifecycle contract
// ------------------
//   1. CR create with Cache.Enabled=true → reconciler adds finalizer,
//      then ensures a single pour+pinned bead exists and stamps its ID
//      onto Status.PinnedIdentityBeadID.
//   2. Subsequent reconciles are idempotent; the stamped ID is reused
//      when the underlying bead still exists, and a new bead is created
//      only if the stamped ID can no longer be resolved (db reset).
//   3. CR delete → the pinned-identity finalizer closes every open wisp
//      whose `caused-by` edge points at the pinned bead, THEN closes
//      the pinned bead, THEN removes itself. Wisp-then-pinned ordering
//      is load-bearing (W1 in the design): closing the pinned first
//      would leave dangling wisp edges.
package controllers

import (
	"context"
	"fmt"
	"time"

	"github.com/steveyegge/beads"

	spirev1 "github.com/awell-health/spire/operator/api/v1alpha1"
	"github.com/awell-health/spire/pkg/store"
)

// pinnedIdentityFinalizer is the finalizer the operator adds to a
// WizardGuild that has a Cache spec, so the bead-graph cleanup runs
// before the CR is removed. Domain matches existing finalizers in this
// project (spire.awell.io/...).
const pinnedIdentityFinalizer = "spire.awell.io/pinned-identity"

// pinnedIdentityStore is the narrow store surface the pinned-identity
// lifecycle needs. The default implementation delegates to the
// pkg/store package-level functions; tests substitute an in-memory
// fake to exercise create/close/list paths without booting dolt.
type pinnedIdentityStore interface {
	GetBead(ctx context.Context, id string) (store.Bead, error)
	CreateBead(ctx context.Context, opts store.CreateOpts) (string, error)
	UpdateBead(ctx context.Context, id string, updates map[string]interface{}) error
	CloseBead(ctx context.Context, id string) error
	GetDependentsWithMeta(ctx context.Context, id string) ([]*beads.IssueWithDependencyMetadata, error)
}

// defaultPinnedIdentityStore wraps pkg/store package-level functions
// behind the pinnedIdentityStore interface. The pkg/store calls carry
// their own context internally — ctx here is accepted for forward
// compatibility but not threaded through.
type defaultPinnedIdentityStore struct{}

func (defaultPinnedIdentityStore) GetBead(_ context.Context, id string) (store.Bead, error) {
	return store.GetBead(id)
}

func (defaultPinnedIdentityStore) CreateBead(_ context.Context, opts store.CreateOpts) (string, error) {
	return store.CreateBead(opts)
}

func (defaultPinnedIdentityStore) UpdateBead(_ context.Context, id string, updates map[string]interface{}) error {
	return store.UpdateBead(id, updates)
}

func (defaultPinnedIdentityStore) CloseBead(_ context.Context, id string) error {
	return store.CloseBead(id)
}

func (defaultPinnedIdentityStore) GetDependentsWithMeta(_ context.Context, id string) ([]*beads.IssueWithDependencyMetadata, error) {
	return store.GetDependentsWithMeta(id)
}

// ensurePinnedIdentity returns the ID of the pour+pinned identity bead
// for guild's cache, creating it if absent. Idempotent: when the
// stamped Status.PinnedIdentityBeadID still resolves to a real bead in
// the store, that ID is returned and no new bead is created. When the
// stamped ID can no longer be resolved (db reset, manual deletion), a
// fresh bead is created and the new ID is returned — the caller is
// responsible for re-stamping Status.
func ensurePinnedIdentity(ctx context.Context, st pinnedIdentityStore, guild *spirev1.WizardGuild) (string, error) {
	if id := guild.Status.PinnedIdentityBeadID; id != "" {
		if _, err := st.GetBead(ctx, id); err == nil {
			return id, nil
		}
		// Stamped ID no longer exists (db reset, manual delete) — fall
		// through and recreate. We treat any GetBead error as
		// "missing"; the alternative is leaving the operator unable to
		// reconcile until the bead reappears, which is worse for a
		// pure-identity marker.
	}

	opts := store.CreateOpts{
		Title:       pinnedIdentityTitle(guild),
		Type:        beads.TypeTask,
		Priority:    4, // non-actionable
		Labels:      pinnedIdentityLabels(guild),
		Description: pinnedIdentityBody(guild),
	}
	id, err := st.CreateBead(ctx, opts)
	if err != nil {
		return "", fmt.Errorf("create pinned identity bead: %w", err)
	}

	// Flip the bead to pinned status + pinned flag. CreateBead in
	// pkg/store hardcodes Status=open and does not surface the Pinned
	// column, so we make the transition in a follow-up update. Both
	// fields are flipped together: the design (Q2) calls for setting
	// the bead status AND the pinned flag so query tooling that
	// inspects either signal classifies this bead correctly.
	updates := map[string]interface{}{
		"status": string(beads.Status("pinned")),
		"pinned": true,
	}
	if err := st.UpdateBead(ctx, id, updates); err != nil {
		return "", fmt.Errorf("flip pinned identity bead %s to pinned: %w", id, err)
	}
	return id, nil
}

// finalizePinnedIdentity closes all open wisps caused-by the guild's
// pinned identity bead, then closes the pinned bead itself.
//
// Ordering matters: closing pinned before its dependent wisps would
// leave the wisps with a `caused-by` edge to a closed bead, breaking
// the cleric's traversal contract (design W1). Errors closing any wisp
// abort before the pinned close runs, so partial-failure leaves the
// pinned bead — and therefore the finalizer — in place; the next
// reconcile will retry.
//
// A guild whose Status.PinnedIdentityBeadID is empty is treated as a
// no-op: either ensurePinnedIdentity never ran, or the cleanup already
// completed (the operator clears the field once the pinned close
// succeeds via Status update on the next reconcile, but the finalizer
// removal on the same reconcile is what makes the field's emptiness
// reachable).
func finalizePinnedIdentity(ctx context.Context, st pinnedIdentityStore, guild *spirev1.WizardGuild) error {
	pinnedID := guild.Status.PinnedIdentityBeadID
	if pinnedID == "" {
		return nil
	}

	wisps, err := listOpenWispsTargeting(ctx, st, pinnedID)
	if err != nil {
		return fmt.Errorf("list open wisps for %s: %w", pinnedID, err)
	}
	for _, w := range wisps {
		if err := st.CloseBead(ctx, w); err != nil {
			return fmt.Errorf("close wisp %s: %w", w, err)
		}
	}

	if err := st.CloseBead(ctx, pinnedID); err != nil {
		return fmt.Errorf("close pinned identity %s: %w", pinnedID, err)
	}
	return nil
}

// listOpenWispsTargeting returns IDs of open wisp beads whose
// `caused-by` edge points at pinnedID. The wisp filter is two-pronged:
// the dependency type pins us to causality edges (not blocks/related),
// and the Ephemeral bool plus non-closed status narrow to wisps that
// still need closing. Beads with caused-by edges that aren't wisps
// (e.g. a regular bug bead authored against this resource) are
// deliberately not closed by the finalizer — those have their own
// lifecycle.
func listOpenWispsTargeting(ctx context.Context, st pinnedIdentityStore, pinnedID string) ([]string, error) {
	deps, err := st.GetDependentsWithMeta(ctx, pinnedID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(deps))
	for _, d := range deps {
		if string(d.DependencyType) != store.DepCausedBy {
			continue
		}
		if !d.Ephemeral {
			continue
		}
		if d.Status == beads.StatusClosed {
			continue
		}
		out = append(out, d.ID)
	}
	return out, nil
}

// pinnedIdentityTitle returns the canonical title for a guild's cache
// pinned identity bead. Format is stable so post-GC analytics that
// rediscover beads by title prefix continue to work.
func pinnedIdentityTitle(g *spirev1.WizardGuild) string {
	return fmt.Sprintf("WizardGuild/%s/Cache", g.Name)
}

// pinnedIdentityLabels returns the bead-graph labels stamped on the
// pinned identity bead. The "resource" + "guild" pair lets queries
// rediscover the bead by spec (rather than by Status-stamped ID),
// which matters when Status is wiped and the operator has to reconcile
// from scratch.
func pinnedIdentityLabels(g *spirev1.WizardGuild) []string {
	return []string{
		"pinned-identity",
		"resource:wizardguild-cache",
		"guild:" + g.Name,
		"owner-uid:" + string(g.UID),
	}
}

// pinnedIdentityBody returns the immutable description body. The body
// is informational — operators reading the bead in tooling get the
// resource URI, the owning guild UID, and the provisioning timestamp,
// plus a notice that the operator owns the lifecycle.
func pinnedIdentityBody(g *spirev1.WizardGuild) string {
	return fmt.Sprintf(`Pinned identity bead for WizardGuild/%s/Cache.

Resource URI : wizardguild/%s/cache
Guild UID    : %s
Provisioned  : %s

This bead exists as the stable target for wisp recovery beads filed
against this resource. It is immutable and non-actionable. The
operator owns its lifecycle — do not edit or close it manually.
`, g.Name, g.Name, g.UID, time.Now().UTC().Format(time.RFC3339))
}
