package postgres

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/fin-assist/analytics/internal/domain"
)

var errStoredRevisions = errors.New("invalid stored source revisions")

func revisionsJSON(values domain.Revisions) []byte {
	if len(values) == 0 {
		return nil
	}
	// A validated map of ASCII strings to int64 has no failing JSON encoding path.
	data, _ := json.Marshal(values)
	return data
}
func revisionsFromJSON(data []byte) (domain.Revisions, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
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
