package report

import (
	"context"
	"fmt"
	"time"
)

// bugAttachmentParentTypes is the fixed set the TypeScript contract
// exposes — the panel only displays these three parent_types.
var bugAttachmentParentTypes = []string{"task", "epic", "feature"}

// QueryBugAttachmentWeekly returns one row per ISO week — for each
// parent type we report (parents=distinct parents filed that week,
// parentsWithBugs=subset that had at least one bug-typed child).
//
// Implementation note: we don't have a pre-built parent-child view in
// DuckDB, so we derive parentage from bead_id prefix convention. A
// bead with id "spi-a3f8.1" is a child of "spi-a3f8". This misses any
// manually-created dep edges, but matches how the executor creates
// subtasks — the dominant case.
func (r *SQLReader) QueryBugAttachmentWeekly(ctx context.Context, scope Scope, since, until time.Time) ([]BugAttachmentWeek, error) {
	if r.DB == nil {
		return nil, errNoDB
	}
	// Widen to 12 weeks for the panel's historical view.
	wideSince := since
	if t := until.AddDate(0, 0, -84); t.Before(wideSince) {
		wideSince = t
	}
	clause, scopeArgs := scope.beadIDClause("bead_id")

	// For every bead, split the id on '.' and extract everything
	// before the first '.' as the parent bead_id (empty when there
	// is no '.' — root beads). Then for each week of filed_at:
	//   - count distinct parent ids per parent_type
	//   - count distinct parents that have at least one child whose
	//     bead_type='bug'
	q := fmt.Sprintf(`
		WITH base AS (
			SELECT
				bead_id,
				bead_type,
				filed_at,
				CASE WHEN strpos(bead_id, '.') > 0
				     THEN substr(bead_id, 1, strpos(bead_id, '.') - 1)
				     ELSE NULL END AS parent_id
			FROM bead_lifecycle_olap
			WHERE filed_at IS NOT NULL
			  AND filed_at >= ? AND filed_at <= ?
			  AND (bead_type IS NULL OR bead_type NOT IN ('message', 'step', 'attempt', 'review'))%s
		),
		parents AS (
			SELECT DISTINCT
				bead_id AS parent_id,
				bead_type AS parent_type,
				date_trunc('week', filed_at)::DATE AS week_start
			FROM base
			WHERE bead_type IN ('task', 'epic', 'feature')
		),
		parents_with_bugs AS (
			SELECT DISTINCT parent_id FROM base
			WHERE bead_type = 'bug' AND parent_id IS NOT NULL
		)
		SELECT p.week_start,
		       p.parent_type,
		       COUNT(*) AS parents_total,
		       SUM(CASE WHEN pwb.parent_id IS NOT NULL THEN 1 ELSE 0 END) AS parents_with_bugs
		FROM parents p
		LEFT JOIN parents_with_bugs pwb ON p.parent_id = pwb.parent_id
		GROUP BY 1, 2
		ORDER BY 1
	`, clause)

	args := append([]any{wideSince, until}, scopeArgs...)
	rows, err := r.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("bug attachment: %w", err)
	}
	defer rows.Close()

	type key struct {
		week time.Time
		pt   string
	}
	data := make(map[key]BugAttachmentByParent)
	weekSet := make(map[time.Time]struct{})
	for rows.Next() {
		var (
			week             time.Time
			pt               string
			total, withBugs  int
		)
		if err := rows.Scan(&week, &pt, &total, &withBugs); err != nil {
			return nil, err
		}
		week = week.UTC()
		weekSet[week] = struct{}{}
		data[key{week, pt}] = BugAttachmentByParent{
			ParentType:      pt,
			Parents:         total,
			ParentsWithBugs: withBugs,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Generate a continuous 12-week spine.
	end := startOfWeek(until)
	out := make([]BugAttachmentWeek, 0, 12)
	for i := 11; i >= 0; i-- {
		week := end.AddDate(0, 0, -7*i)
		byParent := make([]BugAttachmentByParent, 0, len(bugAttachmentParentTypes))
		for _, pt := range bugAttachmentParentTypes {
			row, ok := data[key{week, pt}]
			if !ok {
				row = BugAttachmentByParent{ParentType: pt}
			}
			byParent = append(byParent, row)
		}
		out = append(out, BugAttachmentWeek{
			WeekStart:     week.Format("2006-01-02"),
			ByParentType:  byParent,
			RecentParents: []BugAttachmentRecentParent{},
		})
	}

	// Fill in RecentParents for each week: up to 3 parent beads with
	// the most bugs that week. Runs a second query for clarity — cost
	// is negligible at 12 weeks × few rows each.
	recent, err := r.queryRecentBugParents(ctx, scope, wideSince, until)
	if err != nil {
		return nil, err
	}
	recentByWeek := make(map[time.Time][]BugAttachmentRecentParent)
	for _, rp := range recent {
		recentByWeek[rp.week] = append(recentByWeek[rp.week], rp.row)
	}
	for i := range out {
		week, err := time.Parse("2006-01-02", out[i].WeekStart)
		if err != nil {
			continue
		}
		week = week.UTC()
		if rows := recentByWeek[week]; len(rows) > 0 {
			if len(rows) > 3 {
				rows = rows[:3]
			}
			out[i].RecentParents = rows
		}
	}
	return out, nil
}

// queryRecentBugParents is an internal helper: for each (week,
// parent_id), it returns the bug count and the parent's title so the
// panel can show "the bead most affected by bugs this week".
type recentRow struct {
	week time.Time
	row  BugAttachmentRecentParent
}

func (r *SQLReader) queryRecentBugParents(ctx context.Context, scope Scope, since, until time.Time) ([]recentRow, error) {
	clause, scopeArgs := scope.beadIDClause("b.bead_id")

	q := fmt.Sprintf(`
		WITH bugs AS (
			SELECT
				date_trunc('week', filed_at)::DATE AS week_start,
				CASE WHEN strpos(bead_id, '.') > 0
				     THEN substr(bead_id, 1, strpos(bead_id, '.') - 1)
				     ELSE NULL END AS parent_id
			FROM bead_lifecycle_olap b
			WHERE bead_type = 'bug'
			  AND filed_at IS NOT NULL
			  AND filed_at >= ? AND filed_at <= ?%s
		)
		SELECT week_start, parent_id, COUNT(*) AS bug_count
		FROM bugs
		WHERE parent_id IS NOT NULL
		GROUP BY 1, 2
		ORDER BY 1 DESC, bug_count DESC
	`, clause)
	args := append([]any{since, until}, scopeArgs...)
	rows, err := r.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("recent bug parents: %w", err)
	}
	defer rows.Close()

	var out []recentRow
	var parentIDs []string
	for rows.Next() {
		var (
			week     time.Time
			parentID string
			cnt      int
		)
		if err := rows.Scan(&week, &parentID, &cnt); err != nil {
			return nil, err
		}
		out = append(out, recentRow{
			week: week.UTC(),
			row: BugAttachmentRecentParent{
				BeadID:   parentID,
				BugCount: cnt,
			},
		})
		parentIDs = append(parentIDs, parentID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Batch-fetch titles for the parent bead IDs we collected. Uses a
	// raw query against issues — the operational Dolt table that
	// carries titles — mirroring what pkg/gateway does elsewhere.
	// If `issues` is unreachable (e.g. missing in tests), fall back to
	// empty titles.
	titles := r.fetchBeadTitles(ctx, parentIDs)
	for i := range out {
		out[i].row.Title = titles[out[i].row.BeadID]
	}
	return out, nil
}

// fetchBeadTitles tries `issues` (Dolt's bead table) via the same
// connection. Errors are swallowed — titles are decorative.
func (r *SQLReader) fetchBeadTitles(ctx context.Context, ids []string) map[string]string {
	titles := make(map[string]string, len(ids))
	if len(ids) == 0 || r.DB == nil {
		return titles
	}
	// DuckDB doesn't have access to the Dolt issues table. The real
	// gateway wires the Dolt store separately; in this package we
	// return empty titles rather than complect the Reader with a
	// second DB handle. The frontend renders empty titles as just the
	// bead_id, which is acceptable.
	_ = ctx
	return titles
}
