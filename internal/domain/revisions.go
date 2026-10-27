package domain

import "regexp"

// Revisions records known per-source counters; an absent code remains unknown.
// Codes are open and intermediate storage must preserve values it does not interpret.
type Revisions map[string]int64

var revisionCode = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Validate bounds metadata and refuses corrupt counters without defining a closed catalog.
func (r Revisions) Validate() error {
	if len(r) > 32 {
		return invalid("too many source revisions")
	}
	for code, value := range r {
		if !revisionCode.MatchString(code) || value < 0 {
			return invalid("invalid source revision")
		}
	}
	return nil
}

// Intersect retains only codes whose value is known and equal in both sources.
func (r Revisions) Intersect(other Revisions) Revisions {
	out := Revisions{}
	for code, value := range r {
		if v, ok := other[code]; ok && v == value {
			out[code] = value
		}
	}
	return out
}
