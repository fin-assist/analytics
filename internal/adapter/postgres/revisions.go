package postgres

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/fin-assist/analytics/internal/domain"
)

var errStoredRevisions = errors.New("invalid stored source revisions")

// The write SQL wraps this map with its own calc_version parameter atomically.
func revisionsJSON(values domain.Revisions) []byte {
	if len(values) == 0 {
		return nil
	}
	// A validated map of ASCII strings to int64 has no failing JSON encoding path.
	data, _ := json.Marshal(values)
	return data
}
func revisionsFromJSON(data []byte, rowVersion *int32) (domain.Revisions, error) {
	if len(data) == 0 {
		return nil, nil
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) || bytes.Equal(bytes.TrimSpace(data), []byte("{}")) {
		return nil, nil
	}
	var envelope struct {
		Version   json.RawMessage `json:"calc_version"`
		Revisions json.RawMessage `json:"revisions"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		return nil, errStoredRevisions
	}
	var stampedVersion int32
	if bytes.Equal(bytes.TrimSpace(envelope.Version), []byte("null")) || json.Unmarshal(envelope.Version, &stampedVersion) != nil || stampedVersion <= 0 {
		return nil, errStoredRevisions
	}
	// An old binary advances the financial row without touching this column.
	// Such metadata cannot certify the new facts after rolling forward again.
	if rowVersion == nil || stampedVersion != *rowVersion {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if bytes.Equal(bytes.TrimSpace(envelope.Revisions), []byte("null")) || json.Unmarshal(envelope.Revisions, &fields) != nil {
		return nil, errStoredRevisions
	}
	values := make(domain.Revisions, len(fields))
	for code, raw := range fields {
		var value int64
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
			return nil, errStoredRevisions
		}
		values[code] = value
	}
	if values.Validate() != nil {
		return nil, errStoredRevisions
	}
	return values, nil
}
