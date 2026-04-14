package store

import (
	"encoding/json"
	"fmt"

	"github.com/steveyegge/beads"
)

// metadataFromJSON decodes a json.RawMessage into a flat string map.
// Returns nil if the input is nil, empty, or not a JSON object.
func metadataFromJSON(raw json.RawMessage) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	if len(m) == 0 {
		return nil
	}
	result := make(map[string]string, len(m))
	for k, v := range m {
		switch val := v.(type) {
		case string:
			result[k] = val
		case bool:
			if val {
				result[k] = "true"
			} else {
				result[k] = "false"
			}
		case float64:
			result[k] = fmt.Sprintf("%v", val)
		case nil:
			// skip nil values
		default:
			// For complex types, marshal back to JSON string
			b, err := json.Marshal(val)
			if err == nil {
				result[k] = string(b)
			}
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// metadataToJSON encodes a flat string map into a json.RawMessage.
// Returns nil if the map is nil or empty.
func metadataToJSON(m map[string]string) json.RawMessage {
	if len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}

// GetBeadMetadata returns structured metadata for beadID.
func GetBeadMetadata(id string) (map[string]string, error) {
	b, err := GetBead(id)
	if err != nil {
		return nil, err
	}
	return b.Metadata, nil
}

// SetBeadMetadata upserts a single metadata key on a bead via the issue
// metadata JSON field.
func SetBeadMetadata(id, key, value string) error {
	return SetBeadMetadataMap(id, map[string]string{key: value})
}

// SetBeadMetadataMap merges the given key-value pairs into the bead's issue
// metadata and writes the result via UpdateIssue. Existing keys not in m are
// preserved.
func SetBeadMetadataMap(id string, m map[string]string) error {
	if len(m) == 0 {
		return nil
	}
	existing, err := GetBeadMetadata(id)
	if err != nil {
		return err
	}
	merged := make(map[string]string)
	for k, v := range existing {
		merged[k] = v
	}
	for k, v := range m {
		merged[k] = v
	}
	raw := metadataToJSON(merged)
	return UpdateBead(id, map[string]interface{}{"metadata": raw})
}

// appendToStringList parses raw as a JSON string array, appends value if not
// already present, and returns the marshaled result. If raw is empty or
// invalid, a new single-element array is returned.
func appendToStringList(raw, value string) (string, bool) {
	var list []string
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &list); err != nil {
			list = nil
		}
	}
	for _, v := range list {
		if v == value {
			return raw, false // already present
		}
	}
	list = append(list, value)
	out, err := json.Marshal(list)
	if err != nil {
		return raw, false
	}
	return string(out), true
}

// AppendBeadMetadataList appends value to a JSON string array stored under
// the given metadata key. If the key doesn't exist or isn't a valid JSON
// array, a new array is created. Duplicate values are skipped (idempotent).
func AppendBeadMetadataList(id, key, value string) error {
	meta, err := GetBeadMetadata(id)
	if err != nil {
		return fmt.Errorf("read metadata for %s: %w", id, err)
	}
	existing := ""
	if meta != nil {
		existing = meta[key]
	}
	updated, changed := appendToStringList(existing, value)
	if !changed {
		return nil
	}
	return SetBeadMetadata(id, key, updated)
}

// ListBeadsByMetadata searches for beads whose issue metadata matches the
// given key-value pairs (AND semantics). The optional modFn callbacks allow
// callers to set additional filter fields (status, type, etc.) before the
// query executes.
func ListBeadsByMetadata(meta map[string]string, modFn ...func(*beads.IssueFilter)) ([]Bead, error) {
	filter := beads.IssueFilter{
		MetadataFields: meta,
	}
	for _, fn := range modFn {
		fn(&filter)
	}
	return ListBeads(filter)
}
