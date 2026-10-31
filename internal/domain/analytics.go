package domain

import "time"

// AnalyticsData is one consistent read of a user's state, selected snapshot and neighbours.
// A missing snapshot is distinct from HasData=false: the explicit period can have no coverage.
type AnalyticsData struct {
	SelectedKey    *string
	State          *State
	Snapshot       *Snapshot
	Previous, Next *string
	History        *ForecastHistory
}

// Analytics is the resolved screen input. Presentation belongs to the delivery adapter.
type Analytics struct {
	AnalyticsData
	Key            SnapshotKey
	Range          TimeRange
	Current, Stale bool
}

// ForecastHistory is the proven inventory of monthly coverage, read with the selected screen.
// Nil means delivery is incomplete or legacy metadata cannot prove the inventory.
type ForecastHistory struct {
	Months []HistoryMonth
}

// HistoryMonth contains calendar coverage facts, independent of transfer mode and metric values.
type HistoryMonth struct {
	Key              string
	Range            TimeRange
	Coverage         Coverage
	DataFrom, DataTo *time.Time
	Gaps             []TimeRange
}
