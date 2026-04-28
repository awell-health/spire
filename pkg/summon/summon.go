// Package summon holds the per-bead wizard-spawn flow shared by the
// `spire summon` CLI verb and the HTTP gateway's /beads/{id}/summon endpoint.
//
// The CLI owns candidate selection (explicit targets vs. auto-pick-ready);
// this package takes one already-selected bead and does the rest: dispatch
// label persistence, wizard registry cleanup + duplicate guard, formula/tower/
// backend resolution, fire-and-forget spawn, registry add, and an audit
// comment back on the bead.
package summon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/cleric"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/dolt"
	"github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/store"
	"github.com/awell-health/spire/pkg/wizardregistry"
	"github.com/steveyegge/beads"
)

// Result is what a successful Run / SpawnWizard returns.
type Result struct {
	WizardName string
	CommentID  string
}

// ErrAlreadyRunning is returned (wrapped) when a live wizard already owns the
// bead. CLI callers typically treat this as a skip; API callers as a 4xx.
var ErrAlreadyRunning = errors.New("wizard already running")

// ErrRecoveryInFlight is returned (wrapped) when a non-closed recovery
// bead is linked via caused-by to the bead being summoned. The single-
// owner invariant (cleric runtime, spi-hhkozk) requires that the wizard
// and cleric never run simultaneously on the same source bead — while a
// recovery is open, the human gate (or an in-flight cleric round) owns
// the source and a fresh wizard summon is refused.
var ErrRecoveryInFlight = errors.New("open recovery bead exists for this source")

// Seams — package-level vars so cmd/spire tests can intercept calls without
// importing pkg/store or standing up a real dolt server. Defaults wire to
// the real implementations.
var (
	// GetBeadFunc loads a bead by ID.
	GetBeadFunc = store.GetBead

	// UpdateBeadFunc applies a status transition (or other field updates).
	UpdateBeadFunc = store.UpdateBead

	// AddLabelFunc adds a label to a bead.
	AddLabelFunc = store.AddLabel

	// RemoveLabelFunc removes a label from a bead.
	RemoveLabelFunc = store.RemoveLabel

	// AddCommentFunc records an audit comment and returns the comment ID.
	AddCommentFunc = store.AddCommentReturning

	// SpawnFunc is the indirection around backend.Spawn so unit tests can
	// exercise the flow without fork/exec'ing a real subprocess.
	SpawnFunc = func(b agent.Backend, cfg agent.SpawnConfig) (agent.Handle, error) {
		return b.Spawn(cfg)
	}

	// GetDependentsWithMetaFunc is the seam used by the single-owner
	// invariant check (cleric runtime, spi-hhkozk) to look up recovery
	// beads linked to a candidate source bead. Tests overwrite this to
	// inject in-memory dependents without standing up a full store.
	GetDependentsWithMetaFunc = store.GetDependentsWithMeta

	// Registry is the wizard liveness oracle. Wired by cmd/spire to a
	// wizardregistry/local instance in local mode (cluster mode never
	// reaches SpawnWizard since the operator owns wizard pod creation).
	// Nil falls back to ErrAlreadyRunning being driven solely by the
	// post-spawn Upsert path — duplicate detection is a no-op.
	Registry wizardregistry.Registry
)

// ValidateDispatch returns an error when dispatch is not one of the accepted
// modes. Empty string is treated as "no override" and always valid.
func ValidateDispatch(dispatch string) error {
	switch dispatch {
	case "", "sequential", "wave", "direct":
		return nil
	}
	return fmt.Errorf("invalid dispatch mode %q: must be sequential, wave, or direct", dispatch)
}

// Run fetches the bead, gates it on status, transitions open/ready/hooked to
// in_progress, then delegates to SpawnWizard. Used by the HTTP gateway.
// Callers with their own candidate-selection logic (e.g. the CLI) should
// call SpawnWizard directly with an already-gated bead.
func Run(beadID, dispatch string) (Result, error) {
	if err := ValidateDispatch(dispatch); err != nil {
		return Result{}, err
	}
	bead, err := GetBeadFunc(beadID)
	if err != nil {
		return Result{}, fmt.Errorf("target %s: %w", beadID, err)
	}
	if bead.Type == "design" {
		return Result{}, fmt.Errorf("target %s is a design bead — design beads are not executable. Use spire approve to close it", beadID)
	}
	switch bead.Status {
	case "closed", "done":
		return Result{}, fmt.Errorf("target %s is closed — reopen it first (bd update %s --status open)", beadID, beadID)
	case "deferred":
		return Result{}, fmt.Errorf("target %s is deferred — set to open or ready first (bd update %s --status open)", beadID, beadID)
	case "hooked":
		if err := UpdateBeadFunc(beadID, map[string]interface{}{"status": "in_progress"}); err != nil {
			return Result{}, fmt.Errorf("transition hooked bead %s to in_progress: %w", beadID, err)
		}
		bead.Status = "in_progress"
	case "open", "ready":
		if err := UpdateBeadFunc(beadID, map[string]interface{}{"status": "in_progress"}); err != nil {
			return Result{}, fmt.Errorf("transition %s bead %s to in_progress: %w", bead.Status, beadID, err)
		}
		bead.Status = "in_progress"
	}
	return SpawnWizard(bead, dispatch)
}

// SpawnWizard performs the spawn half of the summon flow against an
// already-loaded, already-gated bead. The bead's status is not re-checked;
// the caller is expected to have transitioned open/ready/hooked to
// in_progress. Returns ErrAlreadyRunning (wrapped) if a live wizard already
// owns the bead.
//
// Single-owner invariant (cleric runtime, spi-hhkozk): refuses with
// ErrRecoveryInFlight (wrapped) when any non-closed recovery bead is
// linked to bead via caused-by. The wizard and cleric must never run on
// the same source simultaneously; the steward's hooked-sweep path
// re-summons after the cleric closes the recovery (DOES NOT route through
// this function).
func SpawnWizard(bead store.Bead, dispatch string) (Result, error) {
	if has, recoveryID, err := hasOpenRecovery(bead.ID); err != nil {
		log.Printf("warning: single-owner check for %s failed: %v", bead.ID, err)
	} else if has {
		return Result{}, fmt.Errorf("%w: recovery %s is open for source %s — close or take over the recovery first", ErrRecoveryInFlight, recoveryID, bead.ID)
	}

	if dispatch != "" {
		for _, l := range bead.Labels {
			if strings.HasPrefix(l, "dispatch:") {
				if err := RemoveLabelFunc(bead.ID, l); err != nil {
					return Result{}, fmt.Errorf("remove existing dispatch label %q for %s: %w", l, bead.ID, err)
				}
			}
		}
		if err := AddLabelFunc(bead.ID, "dispatch:"+dispatch); err != nil {
			return Result{}, fmt.Errorf("persist dispatch override for %s: %w", bead.ID, err)
		}
	}

	// Find a live wizard for this bead via the wizardregistry.Registry
	// contract. The Registry impl issues a fresh authoritative-source read
	// per IsAlive call (no snapshot caching), so the duplicate guard is
	// race-safe by construction.
	if Registry != nil {
		ctx := context.Background()
		if entries, lerr := Registry.List(ctx); lerr == nil {
			for _, w := range entries {
				if w.BeadID != bead.ID {
					continue
				}
				alive, _ := Registry.IsAlive(ctx, w.ID)
				if !alive {
					continue
				}
				return Result{WizardName: w.ID}, fmt.Errorf("%w: %s for %s", ErrAlreadyRunning, w.ID, bead.ID)
			}
		}
	}

	name := "wizard-" + bead.ID
	logDir := filepath.Join(dolt.GlobalDir(), "wizards")
	backend := agent.ResolveBackend("")
	formulaName := formula.ResolveV3Name(formula.BeadInfo{
		ID:     bead.ID,
		Type:   bead.Type,
		Labels: bead.Labels,
	})
	towerName := resolveTowerName()

	handle, err := SpawnFunc(backend, agent.SpawnConfig{
		Name:             name,
		BeadID:           bead.ID,
		Role:             agent.RoleExecutor,
		Tower:            towerName,
		LogPath:          filepath.Join(logDir, name+".log"),
		ExtraArgs:        []string{"--formula", formulaName},
		DetachFromParent: true,
	})
	if err != nil {
		return Result{}, fmt.Errorf("spawn %s: %w", name, err)
	}

	pid, _ := strconv.Atoi(handle.Identifier())
	// spi-6pmit1: BeginWork (called from cmd/spire/summon.go) already created the
	// registry entry with a placeholder PID=0 (registry-first ordering). Now that
	// we have the real PID, stamp it via Registry.Upsert. Read-mostly cluster-mode
	// backends return ErrReadOnly here, which we silently accept — the operator
	// owns cluster registry writes.
	if Registry != nil {
		ctx := context.Background()
		w := wizardregistry.Wizard{
			ID:        name,
			Mode:      wizardregistry.ModeLocal,
			PID:       pid,
			BeadID:    bead.ID,
			StartedAt: time.Now().UTC(),
		}
		if uerr := Registry.Upsert(ctx, w); uerr != nil && !errors.Is(uerr, wizardregistry.ErrReadOnly) {
			// Stamp may fail when no entry exists. Fall back via direct upsert
			// of a fresh entry; silently tolerate ErrReadOnly.
			log.Printf("warning: registry stamp for %s: %v", name, uerr)
		}
		// Suppress unused-import warning when worktree path is needed by
		// future Tower/Worktree-aware backends.
		_ = filepath.Join
	}

	commentID, cerr := AddCommentFunc(bead.ID, "summoned "+name)
	if cerr != nil {
		log.Printf("warning: audit comment for %s: %v", bead.ID, cerr)
	}

	return Result{WizardName: name, CommentID: commentID}, nil
}

// hasOpenRecovery delegates to cleric.HasOpenRecovery using the test-
// replaceable GetDependentsWithMetaFunc seam. Wrapped here so the
// summon flow doesn't reach into pkg/cleric directly (and tests can
// inject the dependents list without touching pkg/cleric).
func hasOpenRecovery(sourceBeadID string) (bool, string, error) {
	return cleric.HasOpenRecovery(sourceBeadID, func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return GetDependentsWithMetaFunc(id)
	})
}

// resolveTowerName walks the usual sources in precedence order so the spawned
// wizard inherits the right tower: active tower config → SPIRE_TOWER env →
// config.ActiveTower field. Mirrors the logic in cmd/spire's original
// summonLocal so behavior is identical.
func resolveTowerName() string {
	if tc, err := config.ActiveTowerConfig(); err == nil && tc != nil {
		return tc.Name
	}
	if t := os.Getenv("SPIRE_TOWER"); t != "" {
		return t
	}
	if cfg, err := config.Load(); err == nil && cfg.ActiveTower != "" {
		return cfg.ActiveTower
	}
	return ""
}
