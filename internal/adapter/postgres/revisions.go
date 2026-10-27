package postgres

import (
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
	var values domain.Revisions
	if json.Unmarshal(data, &values) != nil || values.Validate() != nil {
		return nil, errStoredRevisions
	}
	return values, nil
}
