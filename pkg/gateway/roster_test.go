package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/board"
	"github.com/awell-health/spire/pkg/config"
)

// TestCleanDeadLocalWizards covers the filtering logic that prunes registry
// entries whose PID is no longer running. The closure inside handleRoster
// delegates here so this test is what guards the local-native roster
// against zombie wizards reappearing on the desktop.
func TestCleanDeadLocalWizards(t *testing.T) {
	tests := []struct {
		name  string
		in    []board.LocalAgent
		alive func(int) bool
		want  []string // names of survivors, in input order
	}{
		{
			name:  "nil input returns nil",
			in:    nil,
			alive: func(int) bool { return true },
			want:  nil,
		},
		{
			name:  "empty slice returns nil",
			in:    []board.LocalAgent{},
			alive: func(int) bool { return true },
			want:  nil,
		},
		{
			name: "all alive survive in original order",
			in: []board.LocalAgent{
				{Name: "w1", PID: 1001},
				{Name: "w2", PID: 1002},
				{Name: "w3", PID: 1003},
			},
			alive: func(int) bool { return true },
			want:  []string{"w1", "w2", "w3"},
		},
		{
			name: "all dead are dropped",
			in: []board.LocalAgent{
				{Name: "w1", PID: 1001},
				{Name: "w2", PID: 1002},
			},
			alive: func(int) bool { return false },
			want:  nil,
		},
		{
			name: "mixed alive and dead — only alive survive",
			in: []board.LocalAgent{
				{Name: "alive-1", PID: 100},
				{Name: "dead-1", PID: 200},
				{Name: "alive-2", PID: 300},
				{Name: "dead-2", PID: 400},
			},
			alive: func(pid int) bool { return pid == 100 || pid == 300 },
			want:  []string{"alive-1", "alive-2"},
		},
		{
			name: "PID==0 is treated as dead even if probe reports alive",
			in: []board.LocalAgent{
				{Name: "no-pid", PID: 0},
				{Name: "alive", PID: 42},
			},
			alive: func(int) bool { return true },
			want:  []string{"alive"},
		},
		{
			name: "negative PID is treated as dead even if probe reports alive",
			in: []board.LocalAgent{
				{Name: "alive", PID: 42},
				{Name: "negative", PID: -1},
			},
			alive: func(int) bool { return true },
			want:  []string{"alive"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := cleanDeadLocalWizards(tc.in, tc.alive)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d entries, want %d (got=%+v)", len(got), len(tc.want), got)
			}
			for i, name := range tc.want {
				if got[i].Name != name {
					t.Errorf("got[%d].Name = %q, want %q", i, got[i].Name, name)
				}
			}
		})
	}
}

// TestCleanDeadLocalWizards_PIDPassedToProbe verifies the probe is called
// with each entry's PID — guards against future refactors that accidentally
// pass the wrong field (e.g., index, slice position).
func TestCleanDeadLocalWizards_PIDPassedToProbe(t *testing.T) {
	in := []board.LocalAgent{
		{Name: "w1", PID: 111},
		{Name: "w2", PID: 222},
		{Name: "w3", PID: 333},
	}
	var seen []int
	alive := func(pid int) bool {
		seen = append(seen, pid)
		return true
	}
	cleanDeadLocalWizards(in, alive)

	want := []int{111, 222, 333}
	if len(seen) != len(want) {
		t.Fatalf("probe called %d times, want %d (seen=%v)", len(seen), len(want), seen)
	}
	for i, pid := range want {
		if seen[i] != pid {
			t.Errorf("seen[%d] = %d, want %d", i, seen[i], pid)
		}
	}
}

// TestDefaultRosterDeps_LoadSaveRoundTripWithFakeRegistry exercises the
// LoadWizardRegistry / SaveWizardRegistry closures wired by handleRoster
// against a temp-dir-backed wizards.json. This is the integration test the
// review feedback called for: it would catch a regression where the closures
// stop talking to agent.LoadRegistry/SaveRegistry (e.g., reverted to nil
// stubs, or pointed at the wrong registry path).
func TestDefaultRosterDeps_LoadSaveRoundTripWithFakeRegistry(t *testing.T) {
	t.Setenv("SPIRE_CONFIG_DIR", t.TempDir())

	deps := defaultRosterDeps()

	if deps.LoadWizardRegistry == nil || deps.SaveWizardRegistry == nil {
		t.Fatal("defaultRosterDeps returned nil Load/Save closures — handleRoster wiring is broken")
	}
	if deps.CleanDeadWizards == nil || deps.ProcessAlive == nil {
		t.Fatal("defaultRosterDeps returned nil CleanDeadWizards/ProcessAlive — handleRoster wiring is broken")
	}

	// Empty registry round-trip: Load on a fresh config dir returns no entries.
	got, err := deps.LoadWizardRegistry()
	if err != nil {
		t.Fatalf("fresh registry: LoadWizardRegistry returned error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("fresh registry: LoadWizardRegistry = %d entries, want 0 (%+v)", len(got), got)
	}

	// Save populated registry; Load should return what we wrote.
	want := []board.LocalAgent{
		{Name: "wizard-spi-aaa", PID: 4321, BeadID: "spi-aaa", Worktree: "/tmp/aaa"},
		{Name: "wizard-spi-bbb", PID: 5678, BeadID: "spi-bbb", Worktree: "/tmp/bbb"},
	}
	deps.SaveWizardRegistry(want)

	got, err = deps.LoadWizardRegistry()
	if err != nil {
		t.Fatalf("after Save: LoadWizardRegistry returned error: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("after Save: LoadWizardRegistry = %d entries, want %d (got=%+v)", len(got), len(want), got)
	}

	// Sort both by Name so the comparison is order-stable regardless of
	// how agent.Registry serialises the slice.
	sort.Slice(got, func(i, j int) bool { return got[i].Name < got[j].Name })
	sort.Slice(want, func(i, j int) bool { return want[i].Name < want[j].Name })
	for i := range want {
		if got[i].Name != want[i].Name {
			t.Errorf("entry[%d].Name = %q, want %q", i, got[i].Name, want[i].Name)
		}
		if got[i].PID != want[i].PID {
			t.Errorf("entry[%d].PID = %d, want %d", i, got[i].PID, want[i].PID)
		}
		if got[i].BeadID != want[i].BeadID {
			t.Errorf("entry[%d].BeadID = %q, want %q", i, got[i].BeadID, want[i].BeadID)
		}
		if got[i].Worktree != want[i].Worktree {
			t.Errorf("entry[%d].Worktree = %q, want %q", i, got[i].Worktree, want[i].Worktree)
		}
	}
}

// TestDefaultRosterDeps_WiresAgainstSharedRegistry confirms the closures
// returned by defaultRosterDeps target the same on-disk file used by the
// CLI roster (pkg/agent.LoadRegistry/SaveRegistry). If a future refactor
// switches the gateway to a different registry source, this test will
// catch the divergence so /api/v1/roster and `spire roster` cannot drift
// out of sync.
func TestDefaultRosterDeps_WiresAgainstSharedRegistry(t *testing.T) {
	t.Setenv("SPIRE_CONFIG_DIR", t.TempDir())

	// Write directly through the agent package — this is the path the CLI
	// uses. The gateway's deps must observe the same data.
	agent.SaveRegistry(agent.Registry{Wizards: []agent.Entry{
		{Name: "shared-wizard", PID: os.Getpid(), BeadID: "spi-shared"},
	}})

	deps := defaultRosterDeps()
	got, err := deps.LoadWizardRegistry()
	if err != nil {
		t.Fatalf("LoadWizardRegistry returned error: %v", err)
	}
	if len(got) != 1 || got[0].Name != "shared-wizard" || got[0].BeadID != "spi-shared" {
		t.Fatalf("gateway deps did not observe shared registry: got=%+v", got)
	}

	// Round-trip the other direction: gateway saves, agent loads.
	deps.SaveWizardRegistry([]board.LocalAgent{
		{Name: "gateway-wrote", PID: 9999, BeadID: "spi-gw"},
	})
	reg := agent.LoadRegistry()
	if len(reg.Wizards) != 1 || reg.Wizards[0].Name != "gateway-wrote" {
		t.Fatalf("agent registry did not observe gateway save: %+v", reg.Wizards)
	}
}

// TestHandleRoster_DispatchByMode pins the deployment-mode switch in
// handleRoster: each tower mode routes to a distinct response shape.
// This is the spi-rx6bf6 acceptance encoded — the gateway must NEVER
// fall back to LegacyAgentRegistrationBeads for any mode, and must
// honour the active tower's declared mode rather than probing the
// environment.
func TestHandleRoster_DispatchByMode(t *testing.T) {
	tests := []struct {
		name        string
		mode        config.DeploymentMode
		towerErr    error
		wantStatus  int
		wantErrSub  string
	}{
		{
			name:       "local-native returns 200 with empty registry",
			mode:       config.DeploymentModeLocalNative,
			wantStatus: http.StatusOK,
		},
		{
			name:       "attached-reserved returns 501 with typed error",
			mode:       config.DeploymentModeAttachedReserved,
			wantStatus: http.StatusNotImplemented,
			wantErrSub: "attached-reserved",
		},
		{
			name:       "unknown mode returns 500 with mode named",
			mode:       "weird-mode",
			wantStatus: http.StatusInternalServerError,
			wantErrSub: "weird-mode",
		},
		{
			name:       "tower resolution failure returns 500",
			towerErr:   errors.New("no tower"),
			wantStatus: http.StatusInternalServerError,
			wantErrSub: "no tower",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SPIRE_CONFIG_DIR", t.TempDir())

			origTower := resolveTowerForRosterFunc
			defer func() { resolveTowerForRosterFunc = origTower }()
			resolveTowerForRosterFunc = func() (*config.TowerConfig, error) {
				if tc.towerErr != nil {
					return nil, tc.towerErr
				}
				return &config.TowerConfig{Name: "test", DeploymentMode: tc.mode}, nil
			}

			s := &Server{dataDir: t.TempDir()}
			req := httptest.NewRequest(http.MethodGet, "/api/v1/roster", nil)
			rec := httptest.NewRecorder()
			s.handleRoster(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantErrSub != "" {
				var body map[string]string
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode body: %v (raw=%s)", err, rec.Body.String())
				}
				if !strings.Contains(body["error"], tc.wantErrSub) {
					t.Errorf("error body %q does not contain %q", body["error"], tc.wantErrSub)
				}
			}
		})
	}
}

// TestHandleRoster_LocalNative_DoesNotFallBackToBeadRegistry is the
// regression pin for the spi-rx6bf6 symptom: a local-native tower
// with an empty wizards.json must NOT surface stale agent-labeled
// beads as "the roster". Empty is empty.
func TestHandleRoster_LocalNative_DoesNotFallBackToBeadRegistry(t *testing.T) {
	t.Setenv("SPIRE_CONFIG_DIR", t.TempDir())

	origTower := resolveTowerForRosterFunc
	defer func() { resolveTowerForRosterFunc = origTower }()
	resolveTowerForRosterFunc = func() (*config.TowerConfig, error) {
		return &config.TowerConfig{Name: "test", DeploymentMode: config.DeploymentModeLocalNative}, nil
	}

	s := &Server{dataDir: t.TempDir()}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/roster", nil)
	rec := httptest.NewRecorder()
	s.handleRoster(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var summary board.RosterSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("decode body: %v (raw=%s)", err, rec.Body.String())
	}
	if summary.Wizards != 0 || len(summary.Agents) != 0 {
		t.Fatalf("empty registry should produce empty roster, got %d wizards / %d agents (%+v)",
			summary.Wizards, len(summary.Agents), summary)
	}
}
