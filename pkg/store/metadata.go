package store

import "strings"

// recoveryMetadataKeys is the set of label-prefix keys treated as structured metadata.
// Extend this set as new metadata keys are defined.
var recoveryMetadataKeys = map[string]bool{
	"failure_class":       true,
	"failure_signature":   true,
	"source_bead":         true,
	"source_formula":      true,
	"source_step":         true,
	"resolution_kind":     true,
	"verification_status": true,
	"learning_key":        true,
	"reusable":            true,
	"resolved_at":         true,
}

// metadataFromLabels extracts structured key:value labels into a map.
// Only keys in recoveryMetadataKeys are included.
func metadataFromLabels(labels []string) map[string]string {
	m := make(map[string]string)
	for _, l := range labels {
		idx := strings.Index(l, ":")
		if idx < 0 {
			continue
		}
		key := l[:idx]
		if recoveryMetadataKeys[key] {
			m[key] = l[idx+1:]
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// GetBeadMetadata returns structured metadata for beadID.
// It fetches the bead and extracts labels whose key is in recoveryMetadataKeys.
func GetBeadMetadata(id string) (map[string]string, error) {
	b, err := GetBead(id)
	if err != nil {
		return nil, err
	}
	return metadataFromLabels(b.Labels), nil
}

// SetBeadMetadata upserts a single structured metadata key on a bead.
// It removes any existing label with the same prefix, then writes key:value.
func SetBeadMetadata(id, key, value string) error {
	b, err := GetBead(id)
	if err != nil {
		return err
	}
	for _, l := range b.Labels {
		if strings.HasPrefix(l, key+":") {
			if rerr := RemoveLabel(id, l); rerr != nil {
				return rerr
			}
			break
		}
	}
	return AddLabel(id, key+":"+value)
}

// SetBeadMetadataMap calls SetBeadMetadata for each key in m.
func SetBeadMetadataMap(id string, m map[string]string) error {
	for k, v := range m {
		if err := SetBeadMetadata(id, k, v); err != nil {
			return err
		}
	}
	return nil
}
