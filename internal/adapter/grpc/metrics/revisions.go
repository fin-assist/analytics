package metrics

import (
	"sort"

	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"

	"github.com/fin-assist/analytics/internal/domain"
)

func revisionsOf(list []*commonv1.DataRevision) (domain.Revisions, error) {
	if len(list) > 32 {
		return nil, invalid("too many source revisions")
	}
	values := domain.Revisions{}
	for _, r := range list {
		if r == nil {
			return nil, invalid("missing source revision")
		}
		if _, duplicate := values[r.Code]; duplicate {
			return nil, invalid("duplicate source revision")
		}
		values[r.Code] = r.Value
	}
	if err := values.Validate(); err != nil {
		return nil, err
	}
	return values, nil
}

func revisionMessages(values domain.Revisions) []*commonv1.DataRevision {
	codes := make([]string, 0, len(values))
	for code := range values {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	out := make([]*commonv1.DataRevision, 0, len(codes))
	for _, code := range codes {
		out = append(out, &commonv1.DataRevision{Code: code, Value: values[code]})
	}
	return out
}
