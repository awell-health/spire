package observability

import (
	"fmt"

	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/olap"
	"github.com/awell-health/spire/pkg/store"
)

// MaxToolCallPageSize caps the page_size query parameter on the gateway
// tool-calls endpoint. The default page size is 200; over-large pages
// risk shipping kilobytes of args JSON across the wire and blowing the
// desktop's row renderer. Callers asking for a larger page get clamped.
const MaxToolCallPageSize = 1000

// DefaultToolCallPageSize is the page size used when the caller does not
// supply one (or asks for 0). Mirrors the bead description's lean answer:
// 200 per page, large enough that most attempts fit in one page, small
// enough that JSON encoding stays cheap.
const DefaultToolCallPageSize = 200

// getAttemptInstanceFunc and queryToolCallsBySession are the function-
// shaped seams ListAttemptToolCalls calls into. Tests stub them out to
// exercise the clamping/branching logic without standing up a real
// store metadata read or OLAP DB. Mirrors the seam pattern used by
// pkg/gateway/attempt.go's listAttemptToolCallsFunc.
var (
	getAttemptInstanceFunc = store.GetAttemptInstance

	queryToolCallsBySession = func(sessionID string, limit, offset int) ([]olap.ToolCallRecord, error) {
		tc, err := config.ActiveTowerConfig()
		if err != nil {
			return nil, fmt.Errorf("active tower: %w", err)
		}
		if tc == nil {
			return nil, fmt.Errorf("no active tower")
		}
		db, err := olap.Open(tc.OLAPPath())
		if err != nil {
			return nil, fmt.Errorf("open olap %s: %w", tc.OLAPPath(), err)
		}
		defer db.Close()
		return db.QueryToolCallsBySession(sessionID, limit, offset)
	}
)

// ListAttemptToolCalls returns per-invocation tool calls captured during
// the attempt with the given attemptID, paginated. The result joins
// span and log signals (tool_spans + tool_events) by session_id, with
// span rows preferred when both exist (since spans carry the rich args
// JSON). When the attempt's session_id is unknown (pre-migration
// attempts that never stamped instance metadata), the function returns
// nil without an error so callers can render an empty list rather than
// fail.
//
// Pagination follows one-based page indexing — page=1 is the first
// page. Page size is clamped to [1, MaxToolCallPageSize]; a page_size
// of 0 falls back to DefaultToolCallPageSize.
func ListAttemptToolCalls(attemptID string, page, pageSize int) ([]olap.ToolCallRecord, error) {
	if pageSize <= 0 {
		pageSize = DefaultToolCallPageSize
	}
	if pageSize > MaxToolCallPageSize {
		pageSize = MaxToolCallPageSize
	}
	if page < 1 {
		page = 1
	}
	offset := (page - 1) * pageSize

	meta, err := getAttemptInstanceFunc(attemptID)
	if err != nil {
		return nil, fmt.Errorf("read attempt instance metadata for %s: %w", attemptID, err)
	}
	if meta == nil || meta.SessionID == "" {
		// Pre-migration attempts have no session_id stamp. Render as
		// empty list rather than fail — the audit surface shouldn't
		// hard-error on legacy data.
		return nil, nil
	}

	rows, err := queryToolCallsBySession(meta.SessionID, pageSize, offset)
	if err != nil {
		return nil, fmt.Errorf("query tool calls for session %s: %w", meta.SessionID, err)
	}
	return rows, nil
}
