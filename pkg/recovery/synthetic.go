package recovery

import (
	"fmt"
	"sort"
	"strings"

	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// SyntheticRecoveryRequest is the input to WriteSyntheticRecovery.
//
// OriginBeadID is the bead the synthetic recovery points at via a
// caused-by dep — either the simulated "interrupted parent" (pour) or a
// pre-existing pinned identity bead (wisp). FailureClass is the
// simulated classification the cleric will read. FailedStep is the
// optional failed-step hint propagated into metadata and labels.
// ExtraLabels are scoped to interrupted:<k>=<v> unless already prefixed.
// Wisp is a passthrough marker for the caller's intent — since at the
// storage layer both pour and wisp produce identical bead shape, it is
// only recorded as a provenance label on the synthetic bead.
type SyntheticRecoveryRequest struct {
	OriginBeadID string
	FailureClass FailureClass
	FailedStep   string
	ExtraLabels  map[string]string
	Wisp         bool
}

// syntheticWriter is the minimal store surface WriteSyntheticRecovery
// exercises. Production wires storeAdapter; tests inject a fake.
type syntheticWriter interface {
	GetBead(id string) (store.Bead, error)
	CreateBead(opts store.CreateOpts) (string, error)
	AddDepTyped(issueID, dependsOnID, depType string) error
	AddLabel(id, label string) error
	SetBeadMetadataMap(id string, meta map[string]string) error
}

type storeSyntheticWriter struct{}

func (storeSyntheticWriter) GetBead(id string) (store.Bead, error) {
	return store.GetBead(id)
}

func (storeSyntheticWriter) CreateBead(opts store.CreateOpts) (string, error) {
	return store.CreateBead(opts)
}

func (storeSyntheticWriter) AddDepTyped(issueID, dependsOnID, depType string) error {
	return store.AddDepTyped(issueID, dependsOnID, depType)
}

func (storeSyntheticWriter) AddLabel(id, label string) error {
	return store.AddLabel(id, label)
}

func (storeSyntheticWriter) SetBeadMetadataMap(id string, meta map[string]string) error {
	return store.SetBeadMetadataMap(id, meta)
}

// WriteSyntheticRecovery files a type=recovery bead whose shape mirrors
// the real-failure escalation path in pkg/executor
// (createOrUpdateRecoveryBead): caused-by edge to origin, recovery-bead
// + failure_class:<class> labels, RecoveryMetadata seeded via
// RecoveryMetadata.Apply. On top of the real-path shape it applies
// interrupted:<k>=<v> labels the cleric can use to distinguish synthetic
// provenance during debug flows.
//
// Returns the new bead's ID. Errors if the origin bead does not exist or
// FailureClass is empty.
func WriteSyntheticRecovery(req SyntheticRecoveryRequest) (string, error) {
	return writeSyntheticRecovery(storeSyntheticWriter{}, req)
}

func writeSyntheticRecovery(w syntheticWriter, req SyntheticRecoveryRequest) (string, error) {
	if req.OriginBeadID == "" {
		return "", fmt.Errorf("synthetic recovery: OriginBeadID required")
	}
	if req.FailureClass == "" {
		return "", fmt.Errorf("synthetic recovery: FailureClass required")
	}

	if _, err := w.GetBead(req.OriginBeadID); err != nil {
		return "", fmt.Errorf("synthetic recovery: origin %s: %w", req.OriginBeadID, err)
	}

	failureType := string(req.FailureClass)

	// Base labels match the real-failure recovery-bead shape verbatim.
	baseLabels := []string{
		"recovery-bead",
		"failure_class:" + failureType,
	}

	// Description carries a [debug] marker so the bead is self-identifying
	// as synthetic on inspection. The cleric reads structured metadata, not
	// the description, so the marker does not affect diagnose output.
	desc := fmt.Sprintf("[debug] failure_class=%s", failureType)
	if req.FailedStep != "" {
		desc += " failed_step=" + req.FailedStep
	}

	title := fmt.Sprintf("[recovery] %s: %s", req.OriginBeadID, failureType)
	if len(title) > 200 {
		title = title[:200]
	}

	newID, err := w.CreateBead(store.CreateOpts{
		Title:       title,
		Description: desc,
		Priority:    1,
		Type:        beads.IssueType("recovery"),
		Labels:      baseLabels,
		Prefix:      store.PrefixFromID(req.OriginBeadID),
	})
	if err != nil {
		return "", fmt.Errorf("synthetic recovery: create bead: %w", err)
	}

	if err := w.AddDepTyped(newID, req.OriginBeadID, "caused-by"); err != nil {
		return newID, fmt.Errorf("synthetic recovery: caused-by dep: %w", err)
	}

	for _, l := range buildInterruptedLabels(failureType, req.FailedStep, req.ExtraLabels) {
		if err := w.AddLabel(newID, l); err != nil {
			return newID, fmt.Errorf("synthetic recovery: add label %s: %w", l, err)
		}
	}

	if req.Wisp {
		if err := w.AddLabel(newID, "synthetic:wisp"); err != nil {
			return newID, fmt.Errorf("synthetic recovery: wisp label: %w", err)
		}
	}

	sig := failureType
	if req.FailedStep != "" {
		sig = failureType + ":" + req.FailedStep
	}
	meta := RecoveryMetadata{
		FailureClass:     failureType,
		SourceBead:       req.OriginBeadID,
		SourceStep:       req.FailedStep,
		FailureSignature: sig,
	}
	if err := w.SetBeadMetadataMap(newID, meta.ToMap()); err != nil {
		return newID, fmt.Errorf("synthetic recovery: apply metadata: %w", err)
	}

	return newID, nil
}

// buildInterruptedLabels composes the interrupted:* label set for a
// synthetic recovery. Ordering is stable (failure-class, failed-step,
// then extras sorted by key) so tests can assert label membership without
// caring about iteration order.
func buildInterruptedLabels(failureClass, failedStep string, extras map[string]string) []string {
	var out []string
	out = append(out, "interrupted:failure-class="+failureClass)
	if failedStep != "" {
		out = append(out, "interrupted:failed-step="+failedStep)
	}

	if len(extras) == 0 {
		return out
	}
	keys := make([]string, 0, len(extras))
	for k := range extras {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := extras[k]
		if strings.HasPrefix(k, "interrupted:") {
			out = append(out, k+"="+v)
			continue
		}
		out = append(out, "interrupted:"+k+"="+v)
	}
	return out
}
