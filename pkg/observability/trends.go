package observability

import (
	"fmt"
	"math"
	"sort"
)

// WeekTrend holds a single week's metric value with week-over-week change.
type WeekTrend struct {
	Week      string  `json:"week"`       // e.g. "2026-W13"
	Value     float64 `json:"value"`      // the metric value for this week
	PrevValue float64 `json:"prev_value"` // prior week's value (NaN if none)
	PctChange float64 `json:"pct_change"` // (value-prev)/prev*100, NaN if no prior
}

// TrendResult holds week-over-week trend data for five key metrics.
type TrendResult struct {
	MergeFrequency []WeekTrend `json:"merge_frequency"` // merges per week
	ReviewFriction []WeekTrend `json:"review_friction"` // avg review_rounds per week
	CostPerMerge   []WeekTrend `json:"cost_per_merge"`  // sum(cost_usd) / merge_count per week
	SuccessRate    []WeekTrend `json:"success_rate"`     // % result='success' per week
	LeadTimeP50    []WeekTrend `json:"lead_time_p50"`    // P50 working_seconds per week (hours)
}

// MetricsTrends computes week-over-week trends for the last N weeks.
// If weeks <= 0 it defaults to 12.
func MetricsTrends(weeks int) (*TrendResult, error) {
	if weeks <= 0 {
		weeks = 12
	}
	// Request extra weeks so LAG has a prior value for the oldest visible week.
	queryWeeks := weeks + 1

	merges, err := queryMergeFrequencyTrend(queryWeeks)
	if err != nil {
		return nil, fmt.Errorf("trends: merge frequency: %w", err)
	}
	friction, err := queryReviewFrictionTrend(queryWeeks)
	if err != nil {
		return nil, fmt.Errorf("trends: review friction: %w", err)
	}
	cost, err := queryCostPerMergeTrend(queryWeeks)
	if err != nil {
		return nil, fmt.Errorf("trends: cost per merge: %w", err)
	}
	success, err := querySuccessRateTrend(queryWeeks)
	if err != nil {
		return nil, fmt.Errorf("trends: success rate: %w", err)
	}
	lead, err := queryLeadTimeP50Trend(queryWeeks)
	if err != nil {
		return nil, fmt.Errorf("trends: lead time p50: %w", err)
	}

	// Trim to requested week count (drop the extra oldest week used for LAG).
	trimTo := func(t []WeekTrend) []WeekTrend {
		if len(t) > weeks {
			return t[:weeks]
		}
		return t
	}

	return &TrendResult{
		MergeFrequency: trimTo(merges),
		ReviewFriction: trimTo(friction),
		CostPerMerge:   trimTo(cost),
		SuccessRate:    trimTo(success),
		LeadTimeP50:    trimTo(lead),
	}, nil
}

// queryMergeFrequencyTrend returns weekly merge counts with WoW change.
func queryMergeFrequencyTrend(weeks int) ([]WeekTrend, error) {
	q := fmt.Sprintf(`
		WITH weekly AS (
			SELECT
				YEARWEEK(completed_at, 3) AS yw,
				DATE_FORMAT(MIN(completed_at), '%%Y-W%%v') AS week_label,
				COUNT(*) AS merges
			FROM agent_runs
			WHERE phase = 'merge'
			  AND result = 'success'
			  AND completed_at >= DATE_SUB(CURDATE(), INTERVAL %d WEEK)
			GROUP BY YEARWEEK(completed_at, 3)
		)
		SELECT
			week_label,
			merges AS val,
			LAG(merges) OVER (ORDER BY yw) AS prev_val
		FROM weekly
		ORDER BY yw DESC
	`, weeks)
	return queryTrendRows(q)
}

// queryReviewFrictionTrend returns weekly avg review rounds with WoW change.
func queryReviewFrictionTrend(weeks int) ([]WeekTrend, error) {
	q := fmt.Sprintf(`
		WITH weekly AS (
			SELECT
				YEARWEEK(completed_at, 3) AS yw,
				DATE_FORMAT(MIN(completed_at), '%%Y-W%%v') AS week_label,
				AVG(review_rounds) AS avg_rounds
			FROM agent_runs
			WHERE review_rounds IS NOT NULL
			  AND review_rounds > 0
			  AND completed_at >= DATE_SUB(CURDATE(), INTERVAL %d WEEK)
			GROUP BY YEARWEEK(completed_at, 3)
		)
		SELECT
			week_label,
			avg_rounds AS val,
			LAG(avg_rounds) OVER (ORDER BY yw) AS prev_val
		FROM weekly
		ORDER BY yw DESC
	`, weeks)
	return queryTrendRows(q)
}

// queryCostPerMergeTrend returns weekly cost per merge with WoW change.
func queryCostPerMergeTrend(weeks int) ([]WeekTrend, error) {
	q := fmt.Sprintf(`
		WITH weekly AS (
			SELECT
				YEARWEEK(completed_at, 3) AS yw,
				DATE_FORMAT(MIN(completed_at), '%%Y-W%%v') AS week_label,
				SUM(COALESCE(cost_usd, 0)) AS total_cost,
				SUM(CASE WHEN phase = 'merge' AND result = 'success' THEN 1 ELSE 0 END) AS merge_count
			FROM agent_runs
			WHERE completed_at >= DATE_SUB(CURDATE(), INTERVAL %d WEEK)
			GROUP BY YEARWEEK(completed_at, 3)
			HAVING merge_count > 0
		)
		SELECT
			week_label,
			(total_cost / merge_count) AS val,
			LAG(total_cost / merge_count) OVER (ORDER BY yw) AS prev_val
		FROM weekly
		ORDER BY yw DESC
	`, weeks)
	return queryTrendRows(q)
}

// querySuccessRateTrend returns weekly success rate (%) with WoW change.
func querySuccessRateTrend(weeks int) ([]WeekTrend, error) {
	q := fmt.Sprintf(`
		WITH weekly AS (
			SELECT
				YEARWEEK(completed_at, 3) AS yw,
				DATE_FORMAT(MIN(completed_at), '%%Y-W%%v') AS week_label,
				SUM(CASE WHEN result = 'success' THEN 1 ELSE 0 END) * 100.0 / COUNT(*) AS success_pct
			FROM agent_runs
			WHERE completed_at >= DATE_SUB(CURDATE(), INTERVAL %d WEEK)
			GROUP BY YEARWEEK(completed_at, 3)
		)
		SELECT
			week_label,
			success_pct AS val,
			LAG(success_pct) OVER (ORDER BY yw) AS prev_val
		FROM weekly
		ORDER BY yw DESC
	`, weeks)
	return queryTrendRows(q)
}

// queryLeadTimeP50Trend returns weekly P50 lead time in hours.
// Since Dolt lacks PERCENTILE_CONT, we fetch per-week working_seconds
// and compute P50 in Go.
func queryLeadTimeP50Trend(weeks int) ([]WeekTrend, error) {
	q := fmt.Sprintf(`
		SELECT
			YEARWEEK(completed_at, 3) AS yw,
			DATE_FORMAT(MIN(completed_at) OVER (PARTITION BY YEARWEEK(completed_at, 3)), '%%Y-W%%v') AS week_label,
			working_seconds
		FROM agent_runs
		WHERE working_seconds IS NOT NULL
		  AND working_seconds > 0
		  AND completed_at >= DATE_SUB(CURDATE(), INTERVAL %d WEEK)
		ORDER BY yw ASC, working_seconds ASC
	`, weeks)

	rows, err := QueryJSON(q)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}

	// Group by week.
	type weekData struct {
		yw    float64
		label string
		vals  []float64
	}
	weekMap := make(map[float64]*weekData)
	var weekOrder []float64
	for _, row := range rows {
		yw := ToFloat(row["yw"])
		ws := ToFloat(row["working_seconds"])
		label := ToString(row["week_label"])
		wd, ok := weekMap[yw]
		if !ok {
			wd = &weekData{yw: yw, label: label}
			weekMap[yw] = wd
			weekOrder = append(weekOrder, yw)
		}
		wd.vals = append(wd.vals, ws)
	}

	// Sort weeks ascending for LAG computation.
	sort.Float64s(weekOrder)

	var trends []WeekTrend
	prevP50 := math.NaN()
	for _, yw := range weekOrder {
		wd := weekMap[yw]
		sort.Float64s(wd.vals)
		p50 := percentile(wd.vals, 0.50) / 3600.0 // convert seconds to hours

		pctChange := math.NaN()
		pv := prevP50
		if !math.IsNaN(pv) && pv > 0 {
			pctChange = (p50 - pv) / pv * 100
		}

		trends = append(trends, WeekTrend{
			Week:      wd.label,
			Value:     p50,
			PrevValue: pv,
			PctChange: pctChange,
		})
		prevP50 = p50
	}

	// Reverse to most-recent-first.
	for i, j := 0, len(trends)-1; i < j; i, j = i+1, j-1 {
		trends[i], trends[j] = trends[j], trends[i]
	}

	return trends, nil
}

// queryTrendRows is a shared helper that runs a trend query returning
// (week_label, val, prev_val) rows and converts them to []WeekTrend.
// Results are ordered most-recent-first (matching the ORDER BY yw DESC).
func queryTrendRows(q string) ([]WeekTrend, error) {
	rows, err := QueryJSON(q)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}

	var trends []WeekTrend
	for _, row := range rows {
		label := ToString(row["week_label"])
		val := ToFloat(row["val"])

		pv := math.NaN()
		pctChange := math.NaN()
		if row["prev_val"] != nil {
			pv = ToFloat(row["prev_val"])
			if pv > 0 {
				pctChange = (val - pv) / pv * 100
			}
		}

		trends = append(trends, WeekTrend{
			Week:      label,
			Value:     val,
			PrevValue: pv,
			PctChange: pctChange,
		})
	}

	return trends, nil
}

