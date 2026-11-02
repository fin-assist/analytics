package domain

import "errors"

// ChartPage holds a bounded projection of immutable all-time monthly points.
type ChartPage struct {
	Details                            []byte
	TimeZone, Revision, Previous, Next string
}

var (
	ErrSourceZone   = errors.New("source timezone unavailable or mixed")
	ErrCursorFormat = errors.New("invalid chart cursor")
	ErrCursorRange  = errors.New("stale chart cursor")
)
