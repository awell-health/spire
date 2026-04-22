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

	if zero.AttemptID != "" {
		t.Errorf("zero AttemptID = %q, want empty", zero.AttemptID)
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
	a := WorkloadIntent{
		AttemptID: "spi-abc123",
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
	}

	b := a
	if a != b {
		t.Errorf("identical WorkloadIntent copies should be equal under ==")
	}

	differentAttempt := a
	differentAttempt.AttemptID = "spi-other"
	if a == differentAttempt {
		t.Errorf("WorkloadIntent values with different AttemptID must not be equal")
	}

	differentPhase := a
	differentPhase.FormulaPhase = "review"
	if a == differentPhase {
		t.Errorf("WorkloadIntent values with different FormulaPhase must not be equal")
	}

	differentRepo := a
	differentRepo.RepoIdentity.URL = "https://example.com/other.git"
	if a == differentRepo {
		t.Errorf("WorkloadIntent values with different RepoIdentity must not be equal")
	}

	differentResources := a
	differentResources.Resources.CPURequest = "1000m"
	if a == differentResources {
		t.Errorf("WorkloadIntent values with different Resources must not be equal")
	}

	differentHandoff := a
	differentHandoff.HandoffMode = "transitional"
	if a == differentHandoff {
		t.Errorf("WorkloadIntent values with different HandoffMode must not be equal")
	}
}

func TestAssignmentIntent_ZeroValue(t *testing.T) {
	var zero AssignmentIntent

	if zero.AttemptID != "" {
		t.Errorf("zero AttemptID = %q, want empty", zero.AttemptID)
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

	want := []string{"AttemptID", "FormulaPhase", "HandoffMode", "RepoIdentity", "Resources"}
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
