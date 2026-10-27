package domain

// AnalyticsData is one consistent read of a user's state, selected snapshot and neighbours.
// A missing snapshot is distinct from HasData=false: the explicit period can have no coverage.
type AnalyticsData struct {
	SelectedKey    *string
	State          *State
	Snapshot       *Snapshot
	Previous, Next *string
}

// Analytics is the resolved screen input. Presentation belongs to the delivery adapter.
type Analytics struct {
	AnalyticsData
	Key            SnapshotKey
	Range          TimeRange
	Current, Stale bool
}
