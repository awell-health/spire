package recovery

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// fakeSyntheticWriter is an in-memory syntheticWriter that captures
// CreateBead / AddDepTyped / AddLabel / SetBeadMetadataMap calls so the
// test can assert on the byte shape a real store would persist.
type fakeSyntheticWriter struct {
	existing map[string]store.Bead

	nextID int

	createdType   beads.IssueType
	createdLabels []string
	createdTitle  string
	createdDesc   string
	createdPrefix string

	deps     []fakeDep
	labels   []string
	metadata map[string]string

	missingOrigin bool
	createErr     error
}

type fakeDep struct {
	issueID     string
	dependsOnID string
	depType     string
}

func (f *fakeSyntheticWriter) GetBead(id string) (store.Bead, error) {
	if f.missingOrigin {
		return store.Bead{}, errors.New("bead not found")
	}
	if b, ok := f.existing[id]; ok {
		return b, nil
	}
	return store.Bead{}, fmt.Errorf("bead %s not found", id)
}

func (f *fakeSyntheticWriter) CreateBead(opts store.CreateOpts) (string, error) {
	if f.createErr != nil {
		return "", f.createErr
	}
	f.nextID++
	id := fmt.Sprintf("spi-syn%d", f.nextID)
	f.createdType = opts.Type
	f.createdLabels = append([]string(nil), opts.Labels...)
	f.createdTitle = opts.Title
	f.createdDesc = opts.Description
	f.createdPrefix = opts.Prefix
	return id, nil
}

func (f *fakeSyntheticWriter) AddDepTyped(issueID, dependsOnID, depType string) error {
	f.deps = append(f.deps, fakeDep{issueID, dependsOnID, depType})
	return nil
}

func (f *fakeSyntheticWriter) AddLabel(id, label string) error {
	f.labels = append(f.labels, label)
	return nil
}

func (f *fakeSyntheticWriter) SetBeadMetadataMap(id string, meta map[string]string) error {
	if f.metadata == nil {
		f.metadata = map[string]string{}
	}
	for k, v := range meta {
		f.metadata[k] = v
	}
	return nil
}

// hasLabel returns true if any AddLabel call wrote this exact label.
func (f *fakeSyntheticWriter) hasLabel(label string) bool {
	for _, l := range f.labels {
		if l == label {
			return true
		}
	}
	return false
}

// hasInitialLabel returns true if the CreateBead opts.Labels slice carried label.
func (f *fakeSyntheticWriter) hasInitialLabel(label string) bool {
	for _, l := range f.createdLabels {
		if l == label {
			return true
		}
	}
	return false
}

// hasCausedBy returns true if any AddDepTyped call recorded
// (issueID, dependsOnID, "caused-by").
func (f *fakeSyntheticWriter) hasCausedBy(issueID, dependsOnID string) bool {
	for _, d := range f.deps {
		if d.issueID == issueID && d.dependsOnID == dependsOnID && d.depType == "caused-by" {
			return true
		}
	}
	return false
}

func newFakeWriter(originID string) *fakeSyntheticWriter {
	return &fakeSyntheticWriter{
		existing: map[string]store.Bead{
			originID: {ID: originID, Type: "task", Status: "awaiting_human"},
		},
	}
}

func TestWriteSyntheticRecovery_MatrixOfFailureClasses(t *testing.T) {
	classes := []FailureClass{
		FailEmptyImplement,
		FailMerge,
		FailBuild,
		FailReviewFix,
		FailRepoResolution,
		FailArbiter,
		FailStepFailure,
		FailUnknown,
	}

	for _, fc := range classes {
		for _, wisp := range []bool{false, true} {
			name := fmt.Sprintf("class=%s/wisp=%t", fc, wisp)
			t.Run(name, func(t *testing.T) {
				origin := "spi-orig1"
				w := newFakeWriter(origin)
				req := SyntheticRecoveryRequest{
					OriginBeadID: origin,
					FailureClass: fc,
					FailedStep:   "implement",
					ExtraLabels:  map[string]string{"reason": "debug"},
					Wisp:         wisp,
				}
				id, err := writeSyntheticRecovery(w, req)
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if id == "" {
					t.Fatal("expected non-empty bead id")
				}

				// Bead type is recovery.
				if string(w.createdType) != "recovery" {
					t.Errorf("created type = %q, want recovery", w.createdType)
				}

				// caused-by dep to origin.
				if !w.hasCausedBy(id, origin) {
					t.Errorf("expected caused-by dep %s -> %s, got deps=%v", id, origin, w.deps)
				}

				// Base labels on the created bead.
				if !w.hasInitialLabel("recovery-bead") {
					t.Errorf("missing recovery-bead base label; got %v", w.createdLabels)
				}
				if !w.hasInitialLabel("failure_class:" + string(fc)) {
					t.Errorf("missing failure_class base label; got %v", w.createdLabels)
				}

				// interrupted:* labels applied post-create.
				wantClassLabel := "interrupted:failure-class=" + string(fc)
				if !w.hasLabel(wantClassLabel) {
					t.Errorf("missing %s; got %v", wantClassLabel, w.labels)
				}
				wantStepLabel := "interrupted:failed-step=implement"
				if !w.hasLabel(wantStepLabel) {
					t.Errorf("missing %s; got %v", wantStepLabel, w.labels)
				}

				// Extra label is propagated under interrupted: scope.
				if !w.hasLabel("interrupted:reason=debug") {
					t.Errorf("extra label not propagated; got %v", w.labels)
				}

				// Wisp provenance label when requested.
				gotWisp := w.hasLabel("synthetic:wisp")
				if gotWisp != wisp {
					t.Errorf("synthetic:wisp label = %v, want %v", gotWisp, wisp)
				}

				// Metadata seeded via RecoveryMetadata.
				if w.metadata[KeyFailureClass] != string(fc) {
					t.Errorf("metadata failure_class = %q, want %q", w.metadata[KeyFailureClass], fc)
				}
				if w.metadata[KeySourceBead] != origin {
					t.Errorf("metadata source_bead = %q, want %q", w.metadata[KeySourceBead], origin)
				}
				if w.metadata[KeySourceStep] != "implement" {
					t.Errorf("metadata source_step = %q, want implement", w.metadata[KeySourceStep])
				}
				wantSig := string(fc) + ":implement"
				if w.metadata[KeyFailureSignature] != wantSig {
					t.Errorf("metadata failure_signature = %q, want %q", w.metadata[KeyFailureSignature], wantSig)
				}

				// Title and prefix line up with the origin.
				if !strings.Contains(w.createdTitle, origin) {
					t.Errorf("title %q does not reference origin %q", w.createdTitle, origin)
				}
				if w.createdPrefix != "spi" {
					t.Errorf("prefix = %q, want spi", w.createdPrefix)
				}

				// Description carries the [debug] provenance marker.
				if !strings.Contains(w.createdDesc, "[debug]") {
					t.Errorf("description %q missing [debug] marker", w.createdDesc)
				}
			})
		}
	}
}

func TestWriteSyntheticRecovery_NoFailedStep(t *testing.T) {
	origin := "spi-orig2"
	w := newFakeWriter(origin)
	req := SyntheticRecoveryRequest{
		OriginBeadID: origin,
		FailureClass: FailMerge,
	}
	id, err := writeSyntheticRecovery(w, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty id")
	}

	// failed-step label is omitted when FailedStep is empty.
	for _, l := range w.labels {
		if strings.HasPrefix(l, "interrupted:failed-step=") {
			t.Errorf("unexpected failed-step label: %s", l)
		}
	}

	// FailureSignature defaults to failure class alone (no step suffix).
	if w.metadata[KeyFailureSignature] != "merge-failure" {
		t.Errorf("failure_signature = %q, want merge-failure", w.metadata[KeyFailureSignature])
	}
	if w.metadata[KeySourceStep] != "" {
		t.Errorf("source_step should be empty; got %q", w.metadata[KeySourceStep])
	}
}

func TestWriteSyntheticRecovery_PrefixedExtraLabelPassthrough(t *testing.T) {
	origin := "spi-orig3"
	w := newFakeWriter(origin)
	req := SyntheticRecoveryRequest{
		OriginBeadID: origin,
		FailureClass: FailBuild,
		ExtraLabels: map[string]string{
			"interrupted:already-scoped": "yes",
			"plain":                      "value",
		},
	}
	if _, err := writeSyntheticRecovery(w, req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !w.hasLabel("interrupted:already-scoped=yes") {
		t.Errorf("prefixed extra label not passed through; got %v", w.labels)
	}
	if !w.hasLabel("interrupted:plain=value") {
		t.Errorf("plain extra label not prefixed; got %v", w.labels)
	}
}

func TestWriteSyntheticRecovery_MissingOrigin(t *testing.T) {
	w := &fakeSyntheticWriter{missingOrigin: true}
	req := SyntheticRecoveryRequest{
		OriginBeadID: "spi-missing",
		FailureClass: FailMerge,
	}
	if _, err := writeSyntheticRecovery(w, req); err == nil {
		t.Fatal("expected error when origin does not exist")
	}
}

func TestWriteSyntheticRecovery_MissingRequiredFields(t *testing.T) {
	cases := []struct {
		name string
		req  SyntheticRecoveryRequest
		want string
	}{
		{
			name: "no origin",
			req:  SyntheticRecoveryRequest{FailureClass: FailMerge},
			want: "OriginBeadID required",
		},
		{
			name: "no failure class",
			req:  SyntheticRecoveryRequest{OriginBeadID: "spi-o"},
			want: "FailureClass required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := writeSyntheticRecovery(newFakeWriter("spi-o"), tc.req)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestBuildInterruptedLabels_Stable(t *testing.T) {
	got := buildInterruptedLabels("merge-failure", "implement", map[string]string{
		"zebra": "z",
		"alpha": "a",
	})
	want := []string{
		"interrupted:failure-class=merge-failure",
		"interrupted:failed-step=implement",
		"interrupted:alpha=a",
		"interrupted:zebra=z",
	}
	if len(got) != len(want) {
		t.Fatalf("len=%d want %d; got=%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q want %q", i, got[i], want[i])
		}
	}
}
