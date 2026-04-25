package metrics

import (
	"strings"
	"testing"
	"time"
)

func TestBuildInsertSQL_AuthProfile(t *testing.T) {
	strPtr := func(s string) *string { return &s }

	base := AgentRun{
		ID:        "run-01020304",
		BeadID:    "spi-abc123",
		Model:     "claude-opus-4-7",
		Role:      "wizard",
		Result:    "success",
		StartedAt: "2026-04-24T10:00:00Z",
	}

	tests := []struct {
		name                   string
		profile                *string
		profileFinal           *string
		wantProfileCol         bool
		wantProfileFinalCol    bool
		wantProfileValue       string
		wantProfileFinalValue  string
	}{
		{
			name:                "both nil — historical row semantic",
			profile:             nil,
			profileFinal:        nil,
			wantProfileCol:      false,
			wantProfileFinalCol: false,
		},
		{
			name:                 "only auth_profile set — no 429 swap",
			profile:              strPtr("subscription"),
			profileFinal:         nil,
			wantProfileCol:       true,
			wantProfileFinalCol:  false,
			wantProfileValue:     "'subscription'",
		},
		{
			name:                  "both set — 429 swap occurred",
			profile:               strPtr("subscription"),
			profileFinal:          strPtr("api-key"),
			wantProfileCol:        true,
			wantProfileFinalCol:   true,
			wantProfileValue:      "'subscription'",
			wantProfileFinalValue: "'api-key'",
		},
		{
			name:                 "empty-string pointer still writes",
			profile:              strPtr(""),
			profileFinal:         nil,
			wantProfileCol:       true,
			wantProfileFinalCol:  false,
			wantProfileValue:     "''",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := base
			run.AuthProfile = tt.profile
			run.AuthProfileFinal = tt.profileFinal
			sql := buildInsertSQL(run)

			hasProfileCol := strings.Contains(sql, "auth_profile,") || strings.Contains(sql, "auth_profile)")
			if hasProfileCol != tt.wantProfileCol {
				t.Errorf("auth_profile column present = %v, want %v\nSQL: %s", hasProfileCol, tt.wantProfileCol, sql)
			}

			hasProfileFinalCol := strings.Contains(sql, "auth_profile_final")
			if hasProfileFinalCol != tt.wantProfileFinalCol {
				t.Errorf("auth_profile_final column present = %v, want %v\nSQL: %s", hasProfileFinalCol, tt.wantProfileFinalCol, sql)
			}

			if tt.wantProfileValue != "" && !strings.Contains(sql, tt.wantProfileValue) {
				t.Errorf("expected auth_profile value %q in SQL: %s", tt.wantProfileValue, sql)
			}
			if tt.wantProfileFinalValue != "" && !strings.Contains(sql, tt.wantProfileFinalValue) {
				t.Errorf("expected auth_profile_final value %q in SQL: %s", tt.wantProfileFinalValue, sql)
			}
		})
	}
}

func TestBuildInsertSQL_AuthProfileEscaping(t *testing.T) {
	// Exotic slot names with apostrophes must be escaped like any other string.
	nasty := "sub'scription"
	run := AgentRun{
		ID:          "run-01020304",
		BeadID:      "spi-abc123",
		Model:       "claude-opus-4-7",
		Role:        "wizard",
		Result:      "success",
		StartedAt:   "2026-04-24T10:00:00Z",
		AuthProfile: &nasty,
	}
	sql := buildInsertSQL(run)
	if !strings.Contains(sql, "'sub''scription'") {
		t.Errorf("expected escaped apostrophe in SQL, got: %s", sql)
	}
}

func TestParseReviewCycleMetrics(t *testing.T) {
	// Helper to build CSV rows. Each row: review_round,review_step,started_at,completed_at,result
	csv := func(rows ...string) string {
		return strings.Join(rows, "\n")
	}
	// row builds a single CSV row with the given fields.
	row := func(round, step, start, end, result string) string {
		return round + "," + step + "," + start + "," + end + "," + result
	}

	t0 := time.Date(2026, 3, 30, 10, 0, 0, 0, time.UTC)
	ts := func(d time.Duration) string { return t0.Add(d).Format(time.RFC3339) }

	tests := []struct {
		name           string
		csvData        string
		wantNil        bool
		wantErr        bool
		wantRounds     int
		wantArbiter    bool
		wantParseErrs  int
		check          func(t *testing.T, m *ReviewCycleMetrics)
	}{
		{
			name:    "empty CSV",
			csvData: "",
			wantNil: true,
		},
		{
			name:    "header only",
			csvData: "review_round,review_step,started_at,completed_at,result",
			wantNil: true,
		},
		{
			name: "single sage round approved",
			csvData: csv(
				row("1", "sage-review", ts(0), ts(5*time.Minute), "approve"),
			),
			wantRounds:  1,
			wantArbiter: false,
			check: func(t *testing.T, m *ReviewCycleMetrics) {
				if m.Rounds[0].SageDuration != 5*time.Minute {
					t.Errorf("SageDuration = %v, want 5m", m.Rounds[0].SageDuration)
				}
				if m.Rounds[0].SageVerdict != "approve" {
					t.Errorf("SageVerdict = %q, want %q", m.Rounds[0].SageVerdict, "approve")
				}
				if m.Rounds[0].FixDuration != 0 {
					t.Errorf("FixDuration = %v, want 0 (no fix this round)", m.Rounds[0].FixDuration)
				}
				if m.TotalDuration != 5*time.Minute {
					t.Errorf("TotalDuration = %v, want 5m", m.TotalDuration)
				}
			},
		},
		{
			name: "multi-round sage+fix",
			csvData: csv(
				row("1", "sage-review", ts(0), ts(3*time.Minute), "request_changes"),
				row("1", "fix", ts(3*time.Minute), ts(8*time.Minute), "success"),
				row("2", "sage-review", ts(8*time.Minute), ts(11*time.Minute), "approve"),
			),
			wantRounds:  2,
			wantArbiter: false,
			check: func(t *testing.T, m *ReviewCycleMetrics) {
				// Round 1
				if m.Rounds[0].SageDuration != 3*time.Minute {
					t.Errorf("Round 1 SageDuration = %v, want 3m", m.Rounds[0].SageDuration)
				}
				if m.Rounds[0].FixDuration != 5*time.Minute {
					t.Errorf("Round 1 FixDuration = %v, want 5m", m.Rounds[0].FixDuration)
				}
				if m.Rounds[0].SageVerdict != "request_changes" {
					t.Errorf("Round 1 SageVerdict = %q, want %q", m.Rounds[0].SageVerdict, "request_changes")
				}
				// Round 2
				if m.Rounds[1].SageDuration != 3*time.Minute {
					t.Errorf("Round 2 SageDuration = %v, want 3m", m.Rounds[1].SageDuration)
				}
				if m.Rounds[1].SageVerdict != "approve" {
					t.Errorf("Round 2 SageVerdict = %q, want %q", m.Rounds[1].SageVerdict, "approve")
				}
				if m.TotalDuration != 11*time.Minute {
					t.Errorf("TotalDuration = %v, want 11m", m.TotalDuration)
				}
			},
		},
		{
			name: "arbiter present",
			csvData: csv(
				row("1", "sage-review", ts(0), ts(2*time.Minute), "request_changes"),
				row("1", "fix", ts(2*time.Minute), ts(6*time.Minute), "success"),
				row("2", "sage-review", ts(6*time.Minute), ts(9*time.Minute), "approve"),
				row("2", "arbiter", ts(9*time.Minute), ts(12*time.Minute), "approve"),
			),
			wantRounds:  2,
			wantArbiter: true,
			check: func(t *testing.T, m *ReviewCycleMetrics) {
				if m.ArbiterDuration != 3*time.Minute {
					t.Errorf("ArbiterDuration = %v, want 3m", m.ArbiterDuration)
				}
				if m.TotalDuration != 12*time.Minute {
					t.Errorf("TotalDuration = %v, want 12m", m.TotalDuration)
				}
			},
		},
		{
			name: "sparse rounds (1 and 3 but not 2)",
			csvData: csv(
				row("1", "sage-review", ts(0), ts(2*time.Minute), "request_changes"),
				row("3", "sage-review", ts(10*time.Minute), ts(13*time.Minute), "approve"),
			),
			wantRounds: 2, // only rounds 1 and 3 have data; round 2 is missing from map
			check: func(t *testing.T, m *ReviewCycleMetrics) {
				if m.Rounds[0].Round != 1 {
					t.Errorf("first round number = %d, want 1", m.Rounds[0].Round)
				}
				if m.Rounds[1].Round != 3 {
					t.Errorf("second round number = %d, want 3", m.Rounds[1].Round)
				}
			},
		},
		{
			name: "with header row",
			csvData: csv(
				"review_round,review_step,started_at,completed_at,result",
				row("1", "sage-review", ts(0), ts(4*time.Minute), "approve"),
			),
			wantRounds: 1,
			check: func(t *testing.T, m *ReviewCycleMetrics) {
				if m.Rounds[0].SageDuration != 4*time.Minute {
					t.Errorf("SageDuration = %v, want 4m", m.Rounds[0].SageDuration)
				}
			},
		},
		{
			name: "malformed round number",
			csvData: csv(
				row("abc", "sage-review", ts(0), ts(2*time.Minute), "approve"),
				row("1", "sage-review", ts(2*time.Minute), ts(5*time.Minute), "approve"),
			),
			wantRounds:    1,
			wantParseErrs: 1,
		},
		{
			name: "malformed started_at timestamp",
			csvData: csv(
				row("1", "sage-review", "not-a-time", ts(2*time.Minute), "approve"),
			),
			wantRounds:    0,
			wantParseErrs: 1,
			check: func(t *testing.T, m *ReviewCycleMetrics) {
				if len(m.Rounds) != 0 {
					t.Errorf("Rounds = %d, want 0 (row should be skipped)", len(m.Rounds))
				}
			},
		},
		{
			name: "malformed completed_at timestamp",
			csvData: csv(
				row("1", "sage-review", ts(0), "bad-time", "approve"),
			),
			wantRounds:    0,
			wantParseErrs: 1,
		},
		{
			name: "short row (fewer than 5 columns)",
			csvData: csv(
				"1,sage-review,2026-03-30T10:00:00Z",
			),
			wantRounds:    0,
			wantParseErrs: 1,
		},
		{
			name: "all rows malformed returns non-nil with parse errors",
			csvData: csv(
				row("abc", "sage-review", ts(0), ts(1*time.Minute), "approve"),
				row("def", "fix", ts(1*time.Minute), ts(2*time.Minute), "success"),
			),
			wantRounds:    0,
			wantParseErrs: 2,
			check: func(t *testing.T, m *ReviewCycleMetrics) {
				if m.TotalDuration != 0 {
					t.Errorf("TotalDuration = %v, want 0", m.TotalDuration)
				}
			},
		},
		{
			name:    "invalid CSV syntax",
			csvData: "\"unclosed quote",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := parseReviewCycleMetrics("spi-test", tt.csvData)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantNil {
				if m != nil {
					t.Fatalf("expected nil, got %+v", m)
				}
				return
			}
			if m == nil {
				t.Fatal("expected non-nil metrics")
			}
			if m.BeadID != "spi-test" {
				t.Errorf("BeadID = %q, want %q", m.BeadID, "spi-test")
			}
			if m.TotalRounds != tt.wantRounds {
				t.Errorf("TotalRounds = %d, want %d", m.TotalRounds, tt.wantRounds)
			}
			if m.HadArbiter != tt.wantArbiter {
				t.Errorf("HadArbiter = %v, want %v", m.HadArbiter, tt.wantArbiter)
			}
			if m.ParseErrors != tt.wantParseErrs {
				t.Errorf("ParseErrors = %d, want %d", m.ParseErrors, tt.wantParseErrs)
			}
			if tt.check != nil {
				tt.check(t, m)
			}
		})
	}
}
