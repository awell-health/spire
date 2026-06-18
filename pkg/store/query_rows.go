package store

import (
	"time"
)

// QueryRows runs a read-only SQL query against the tower's Dolt database via
// the in-process connection pool and returns the rows as generic maps.
//
// Values are normalized to JSON-friendly types so callers see the same shapes
// they previously got from `bd sql --json`: numbers as float64, text/datetimes
// as string, NULL as nil. This lets reporting code (pkg/observability) read
// agent_runs in process instead of shelling out to the `bd` CLI — whose startup
// re-imports the full issues.jsonl into an empty DB on every invocation, a
// ~36k-row no-op upsert storm.
//
// Gateway mode: no client method — fails closed with ErrGatewayUnsupported
// (consistent with the other direct-Dolt readers).
func QueryRows(query string) ([]map[string]any, error) {
	if _, ok := isGatewayMode(); ok {
		return nil, gatewayUnsupportedErr("QueryRows")
	}
	db, err := getDB()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for rows.Next() {
		raw := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		m := make(map[string]any, len(cols))
		for i, c := range cols {
			m[c] = normalizeSQLValue(raw[i])
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// normalizeSQLValue maps raw database/sql driver values to the JSON-friendly
// types reporting accessors expect (string / float64 / bool / nil), so an
// in-process query is a drop-in replacement for `bd sql --json` output
// regardless of the connection's parseTime setting.
func normalizeSQLValue(v any) any {
	switch val := v.(type) {
	case nil:
		return nil
	case []byte:
		return string(val)
	case int64:
		return float64(val)
	case int32:
		return float64(val)
	case time.Time:
		return val.Format("2006-01-02 15:04:05")
	default:
		// float64, bool, string are already JSON-friendly.
		return val
	}
}
