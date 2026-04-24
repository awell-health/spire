package report

// buildAspirational synthesizes the optional overlay panels when the
// caller requested aspirational=true. Only lifecycleByType and
// bugAttachmentWeekly are eligible — hero / DORA / cost numbers must
// stay honest.
//
// Rather than invent numbers from thin air, the overlay fills in the
// three "missing" lifecycle stages (R→S, R→C, S→C) with proportional
// estimates derived from F→R, F→S, F→C when those are present. For
// bug attachment, it copies the live weekly data with empty
// recentParents so the UI can show a smoothed historical shape.
//
// This is a deliberately conservative choice: the frontend badges the
// whole overlay panel, so there's no way to mark individual rows as
// synth — all-or-nothing is the contract.
func buildAspirational(lifecycle []LifecycleByType, bugAttach []BugAttachmentWeek) AspirationalBlock {
	return AspirationalBlock{
		LifecycleByType:     synthesizeLifecycle(lifecycle),
		BugAttachmentWeekly: synthesizeBugAttachment(bugAttach),
	}
}

// synthesizeLifecycle returns a parallel lifecycle tree with any
// missing stages estimated from the ones that ARE populated. The
// heuristic: R→S ≈ F→S − F→R, R→C ≈ F→C − F→R, S→C ≈ F→C − F→S.
// Produces non-negative values so percentiles don't go absurd.
func synthesizeLifecycle(in []LifecycleByType) []LifecycleByType {
	if len(in) == 0 {
		return nil
	}
	out := make([]LifecycleByType, 0, len(in))
	for _, lt := range in {
		stages := make(map[string]LifecycleStageStats, 6)
		for _, s := range lt.Stages {
			stages[s.Stage] = s
		}
		fr, fs, fc := stages["F→R"], stages["F→S"], stages["F→C"]
		rs, haveRS := stages["R→S"]
		rc, haveRC := stages["R→C"]
		sc, haveSC := stages["S→C"]
		if !haveRS || !populated(rs) {
			rs = deriveStage("R→S", fs, fr)
		}
		if !haveRC || !populated(rc) {
			rc = deriveStage("R→C", fc, fr)
		}
		if !haveSC || !populated(sc) {
			sc = deriveStage("S→C", fc, fs)
		}
		out = append(out, LifecycleByType{
			Type: lt.Type,
			Stages: []LifecycleStageStats{
				stages["F→R"], stages["F→S"], stages["F→C"], rs, rc, sc,
			},
		})
	}
	return out
}

// populated reports whether a stage row has any non-zero percentile.
// A stage with all zeros is treated as "missing" so the synth layer
// fills it in.
func populated(s LifecycleStageStats) bool {
	return s.P50 > 0 || s.P75 > 0 || s.P95 > 0 || s.P99 > 0
}

// deriveStage estimates a stage's quantiles as max(a.q - b.q, 0) per
// quantile. The outliers list is empty — aspirational data carries no
// bead IDs since they aren't real.
func deriveStage(name string, a, b LifecycleStageStats) LifecycleStageStats {
	return LifecycleStageStats{
		Stage:    name,
		P50:      nonNeg(a.P50 - b.P50),
		P75:      nonNeg(a.P75 - b.P75),
		P95:      nonNeg(a.P95 - b.P95),
		P99:      nonNeg(a.P99 - b.P99),
		Outliers: []LifecycleOutlier{},
	}
}

// nonNeg clamps negative values to 0 — synth must never produce
// nonsense like p95=-120s.
func nonNeg(v float64) float64 {
	if v < 0 {
		return 0
	}
	return v
}

// synthesizeBugAttachment returns a copy of the weekly bug-attachment
// data with recentParents cleared. A cleaner alternative is to hide
// the live data entirely — but the UI wants SOMETHING in the panel
// when aspirational is on.
func synthesizeBugAttachment(in []BugAttachmentWeek) []BugAttachmentWeek {
	if len(in) == 0 {
		return nil
	}
	out := make([]BugAttachmentWeek, len(in))
	for i, w := range in {
		out[i] = BugAttachmentWeek{
			WeekStart:     w.WeekStart,
			ByParentType:  append([]BugAttachmentByParent(nil), w.ByParentType...),
			RecentParents: []BugAttachmentRecentParent{},
		}
	}
	return out
}
