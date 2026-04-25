package intent

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestWorkloadIntent_ZeroValue(t *testing.T) {
	var zero WorkloadIntent

	if zero.TaskID != "" {
		t.Errorf("zero TaskID = %q, want empty", zero.TaskID)
	}
	if zero.DispatchSeq != 0 {
		t.Errorf("zero DispatchSeq = %d, want 0", zero.DispatchSeq)
	}
	if zero.Reason != "" {
		t.Errorf("zero Reason = %q, want empty", zero.Reason)
	}
	if zero.RepoIdentity != (RepoIdentity{}) {
		t.Errorf("zero RepoIdentity = %+v, want zero", zero.RepoIdentity)
	}
	if zero.FormulaPhase != "" {
		t.Errorf("zero FormulaPhase = %q, want empty", zero.FormulaPhase)
	}
	if zero.Resources != (Resources{}) {
		t.Errorf("zero Resources = %+v, want zero", zero.Resources)
	}
	if zero.HandoffMode != "" {
		t.Errorf("zero HandoffMode = %q, want empty", zero.HandoffMode)
	}
}

func TestWorkloadIntent_Equality(t *testing.T) {
	// Runtime carries a slice and a map, so WorkloadIntent is no longer
	// comparable under ==. The cluster contract crosses the seam by
	// value, and reflect.DeepEqual is the canonical equality predicate
	// for that crossing — these cases pin the field-by-field equality
	// shape using DeepEqual instead of ==.
	a := WorkloadIntent{
		TaskID:      "spi-abc123",
		DispatchSeq: 1,
		Reason:      "fresh",
		RepoIdentity: RepoIdentity{
			URL:        "https://example.com/repo.git",
			BaseBranch: "main",
			Prefix:     "spi",
		},
		FormulaPhase: "implement",
		Resources: Resources{
			CPURequest:    "500m",
			CPULimit:      "1000m",
			MemoryRequest: "256Mi",
			MemoryLimit:   "1Gi",
		},
		HandoffMode: "bundle",
		Role:        RoleApprentice,
		Phase:       PhaseImplement,
		Runtime: Runtime{
			Image:   "spire-agent:dev",
			Command: []string{"spire", "apprentice", "run"},
			Env:     map[string]string{"FOO": "bar"},
		},
	}

	b := a
	if !reflect.DeepEqual(a, b) {
		t.Errorf("identical WorkloadIntent copies should be DeepEqual")
	}

	differentTask := a
	differentTask.TaskID = "spi-other"
	if reflect.DeepEqual(a, differentTask) {
		t.Errorf("WorkloadIntent values with different TaskID must not be DeepEqual")
	}

	differentSeq := a
	differentSeq.DispatchSeq = 2
	if reflect.DeepEqual(a, differentSeq) {
		t.Errorf("WorkloadIntent values with different DispatchSeq must not be DeepEqual")
	}

	differentPhase := a
	differentPhase.FormulaPhase = "review"
	if reflect.DeepEqual(a, differentPhase) {
		t.Errorf("WorkloadIntent values with different FormulaPhase must not be DeepEqual")
	}

	differentRepo := a
	differentRepo.RepoIdentity.URL = "https://example.com/other.git"
	if reflect.DeepEqual(a, differentRepo) {
		t.Errorf("WorkloadIntent values with different RepoIdentity must not be DeepEqual")
	}

	differentResources := a
	differentResources.Resources.CPURequest = "1000m"
	if reflect.DeepEqual(a, differentResources) {
		t.Errorf("WorkloadIntent values with different Resources must not be DeepEqual")
	}

	differentHandoff := a
	differentHandoff.HandoffMode = "transitional"
	if reflect.DeepEqual(a, differentHandoff) {
		t.Errorf("WorkloadIntent values with different HandoffMode must not be DeepEqual")
	}

	differentRole := a
	differentRole.Role = RoleSage
	if reflect.DeepEqual(a, differentRole) {
		t.Errorf("WorkloadIntent values with different Role must not be DeepEqual")
	}

	differentTypedPhase := a
	differentTypedPhase.Phase = PhaseReview
	if reflect.DeepEqual(a, differentTypedPhase) {
		t.Errorf("WorkloadIntent values with different Phase must not be DeepEqual")
	}

	differentRuntime := a
	differentRuntime.Runtime.Image = "spire-agent:other"
	if reflect.DeepEqual(a, differentRuntime) {
		t.Errorf("WorkloadIntent values with different Runtime.Image must not be DeepEqual")
	}
}

func TestAssignmentIntent_ZeroValue(t *testing.T) {
	var zero AssignmentIntent

	if zero.TaskID != "" {
		t.Errorf("zero TaskID = %q, want empty", zero.TaskID)
	}
	if zero.TargetGuild != "" {
		t.Errorf("zero TargetGuild = %q, want empty", zero.TargetGuild)
	}
	if zero.Capabilities != nil {
		t.Errorf("zero Capabilities = %v, want nil", zero.Capabilities)
	}
}

func TestRepoIdentity_ZeroValue(t *testing.T) {
	var zero RepoIdentity
	if zero != (RepoIdentity{URL: "", BaseBranch: "", Prefix: ""}) {
		t.Errorf("zero RepoIdentity = %+v, want all-empty", zero)
	}
}

// TestWorkloadIntent_NoLocalFields enforces the seam-boundary rule: the
// intent struct MUST NOT acquire fields that smuggle machine-local
// workspace state across the scheduler-to-reconciler boundary. If a future
// refactor adds a LocalBindings, LocalPath, or similar field, this test
// fails loudly so the reviewer can push back before the layering breaks.
//
// The check works two ways:
//  1. The list of field names must exactly match the approved set.
//  2. No field name may contain any of the forbidden substrings.
func TestWorkloadIntent_NoLocalFields(t *testing.T) {
	typ := reflect.TypeOf(WorkloadIntent{})

	got := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		got = append(got, typ.Field(i).Name)
	}
	sort.Strings(got)

	want := []string{
		"DispatchSeq",
		"FormulaPhase",
		"HandoffMode",
		"Phase",
		"Reason",
		"RepoIdentity",
		"Resources",
		"Role",
		"Runtime",
		"TaskID",
	}
	sort.Strings(want)

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("WorkloadIntent field set drifted: got %v, want %v", got, want)
	}

	forbidden := []string{"LocalBindings", "LocalPath", "LocalWorkspace", "LocalRepoBinding", "Instances"}
	for _, name := range got {
		for _, bad := range forbidden {
			if strings.Contains(name, bad) {
				t.Errorf("WorkloadIntent has forbidden field %q (contains %q); cluster-native seam must not carry local workspace state", name, bad)
			}
		}
	}
}

// TestRepoIdentity_NoLocalFields guards the embedded RepoIdentity the same
// way. If someone adds a LocalPath here, the intent silently starts
// carrying local workspace state.
func TestRepoIdentity_NoLocalFields(t *testing.T) {
	typ := reflect.TypeOf(RepoIdentity{})

	got := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		got = append(got, typ.Field(i).Name)
	}
	sort.Strings(got)

	want := []string{"BaseBranch", "Prefix", "URL"}
	sort.Strings(want)

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RepoIdentity field set drifted: got %v, want %v", got, want)
	}

	forbidden := []string{"LocalBindings", "LocalPath", "LocalWorkspace", "LocalRepoBinding", "State"}
	for _, name := range got {
		for _, bad := range forbidden {
			if strings.Contains(name, bad) {
				t.Errorf("RepoIdentity has forbidden field %q (contains %q); seam must not carry local workspace state", name, bad)
			}
		}
	}
}

// TestIntentPublisher_InterfaceShape pins the IntentPublisher interface so
// a downstream change doesn't silently alter the contract dispatchers rely
// on. A fake implementation must be assignable to the interface.
func TestIntentPublisher_InterfaceShape(t *testing.T) {
	var _ IntentPublisher = (*fakePublisher)(nil)
}

// TestIntentConsumer_InterfaceShape pins the IntentConsumer interface the
// same way.
func TestIntentConsumer_InterfaceShape(t *testing.T) {
	var _ IntentConsumer = (*fakeConsumer)(nil)
}

type fakePublisher struct{}

func (fakePublisher) Publish(_ context.Context, _ WorkloadIntent) error {
	return nil
}

type fakeConsumer struct{}

func (fakeConsumer) Consume(_ context.Context) (<-chan WorkloadIntent, error) {
	return nil, nil
}
