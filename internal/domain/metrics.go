package domain

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata" // time zone validation must not depend on the zoneinfo of the image or host
)

// ErrInvalid marks a request that breaks a rule the service keeps in code
// (docs/schema.md, «Инварианты в коде сервиса»). For UpsertUserMetrics it is a producer
// error: the whole call is refused with a bare INVALID_ARGUMENT and nothing is written.
var ErrInvalid = errors.New("invalid request")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// PeriodType is the number of finassist.common.v1.PeriodType.
type PeriodType int32

// Known period types; UNSPECIFIED (0) is never stored.
const (
	PeriodMonth          PeriodType = 1
	PeriodQuarter        PeriodType = 2
	PeriodYear           PeriodType = 3
	PeriodRollingYear    PeriodType = 5
	PeriodRollingQuarter PeriodType = 6
)

// String is the bounded metric label of a known calendar or rolling period.
func (t PeriodType) String() string {
	switch t {
	case PeriodMonth:
		return "month"
	case PeriodQuarter:
		return "quarter"
	case PeriodYear:
		return "year"
	case PeriodRollingYear:
		return "rolling_year"
	case PeriodRollingQuarter:
		return "rolling_quarter"
	}
	return "unknown"
}

// Known reports whether t is a type this release stores.
func (t PeriodType) Known() bool {
	return t == PeriodMonth || t == PeriodQuarter || t == PeriodYear || t.Rolling()
}

// TransferMode is the number of finassist.common.v1.TransferMode.
type TransferMode int32

// Transfer modes of a snapshot; UNSPECIFIED (0) is never stored.
const (
	TransferWith    TransferMode = 1
	TransferWithout TransferMode = 2
)

// TransferModes are both modes of a period, in primary key order.
var TransferModes = []TransferMode{TransferWith, TransferWithout}

// Coverage is the number of finassist.common.v1.Coverage.
type Coverage int32

// Coverages a snapshot may carry: it is sent only for a period with covered days.
const (
	CoverageComplete Coverage = 1
	CoveragePartial  Coverage = 2
)

// Metric statuses with a rule of their own; every other number is stored as is.
const (
	statusUnspecified int32 = 0
	// StatusUnavailable is set only by analytics; from the producer it is refused
	// (grpc-api.md 7.3, «Поля, которые заполняет analytics»).
	StatusUnavailable int32 = 5
)

// PeriodKey addresses a period of a user: «2026-09» of a month, «2026-Q3», «2026».
type PeriodKey struct {
	Type PeriodType
	Key  string
}

// SnapshotKey addresses one snapshot: a period in one transfer mode.
type SnapshotKey struct {
	PeriodKey
	Mode TransferMode
}

var periodKeyGrammar = map[PeriodType]*regexp.Regexp{
	PeriodMonth:          regexp.MustCompile(`^[0-9]{4}-(0[1-9]|1[0-2])$`),
	PeriodQuarter:        regexp.MustCompile(`^[0-9]{4}-Q[1-4]$`),
	PeriodYear:           regexp.MustCompile(`^[0-9]{4}$`),
	PeriodRollingYear:    regexp.MustCompile(`^rolling_year$`),
	PeriodRollingQuarter: regexp.MustCompile(`^rolling_quarter$`),
}

// Validate checks the type and the exact grammar of the key: on it rests the equality of
// the byte order of period_key (COLLATE "C") and the calendar order (docs/schema.md).
func (k PeriodKey) Validate() error {
	grammar, ok := periodKeyGrammar[k.Type]
	if !ok {
		return invalid("period type %d", k.Type)
	}
	if !grammar.MatchString(k.Key) {
		return invalid("period key %q of type %s", k.Key, k.Type)
	}
	return nil
}

// TimeRange is a half-open range; both bounds are required where a range is present.
type TimeRange struct {
	Start, End *time.Time
}

func (r TimeRange) validate(what string) error {
	if r.Start == nil || r.End == nil {
		return invalid("%s without a bound", what)
	}
	if !r.Start.Before(*r.End) {
		return invalid("%s does not end after its start", what)
	}
	return nil
}

// Lock is MetricLock without its text: analytics builds the text itself, the producer's
// one is ignored (grpc-api.md 7.3).
type Lock struct {
	Reason              int32
	Required, Available *int32
	AvailableFrom       *time.Time
}

// Basis is MetricBasis; a basis may come without a range.
type Basis struct {
	Range      *TimeRange
	FullMonths int32
}

// Filter is the OperationsFilter of a metric. Only the fields the rules of analytics use
// (docs/calc.md 5) exist: the converter refuses q and the amounts instead of losing them.
type Filter struct {
	From, To                 *time.Time
	CategoryID               *string
	Kind, TransferMode       int32
	Selection, SelectionName *string
}

// Metric is a MetricSnapshot. Enum numbers a newer producer may add are kept as is.
type Metric struct {
	ID                string
	DefinitionVersion int32
	Status            int32
	ValueMinor        *int64
	Range             *TimeRange
	Lock              *Lock
	Basis             *Basis
	Filter            *Filter
	// Details is the stored form of the details oneof (protojson of a Metric holding only
	// it); nil means no details.
	Details []byte
}

// Snapshot is a PeriodSnapshot: the full content of a period in one transfer mode.
type Snapshot struct {
	Revisions        Revisions
	Key              SnapshotKey
	Range            *TimeRange
	Coverage         Coverage
	DataFrom, DataTo *time.Time
	Gaps             []TimeRange
	UnreadLines      int32
	CalculatedAt     *time.Time
	Metrics          []Metric
}

// State is UserDataState.
type State struct {
	Revisions                   Revisions
	HasData                     bool
	DataFrom, DataTo            *time.Time
	LastOperationAt, StaleAfter *time.Time
	FullMonths                  int32
	Recalculating               bool
	TimeZone                    string
	CalculatedAt                *time.Time
}

// Upsert is UpsertUserMetrics: the state, full snapshots and removed periods of one user
// at one calc_version, applied in one transaction.
type Upsert struct {
	UserID    string
	Version   int32
	State     *State // nil: the call carries no state
	Snapshots []Snapshot
	Removed   []PeriodKey
}

var userIDGrammar = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidUserID reports whether id is a UUID in its canonical text form, the form of the
// user_id columns and of the tombstone (grpc-api.md: «сейчас UUIDv7»).
func ValidUserID(id string) bool { return userIDGrammar.MatchString(id) }

// maxGaps is the number of gaps a smallint ordinal can number from zero.
const maxGaps = math.MaxInt16 + 1

// Validate checks every rule of «Инварианты в коде сервиса» that a request can break.
// The first broken rule is returned, wrapped in ErrInvalid.
func (u Upsert) Validate() error {
	if !ValidUserID(u.UserID) {
		return invalid("user_id is not a UUID")
	}
	if u.Version <= 0 {
		return invalid("calc_version %d is not positive", u.Version)
	}
	if u.State != nil {
		if err := u.State.Validate(); err != nil {
			return err
		}
	}
	snapshots := make(map[SnapshotKey]bool, len(u.Snapshots))
	periods := make(map[PeriodKey]bool, len(u.Snapshots))
	for i := range u.Snapshots {
		s := &u.Snapshots[i]
		if err := s.Validate(); err != nil {
			return err
		}
		if snapshots[s.Key] {
			return invalid("snapshot %s %s mode %d twice", s.Key.Type, s.Key.Key, s.Key.Mode)
		}
		snapshots[s.Key] = true
		periods[s.Key.PeriodKey] = true
	}
	removed := make(map[PeriodKey]bool, len(u.Removed))
	for _, r := range u.Removed {
		if err := r.Validate(); err != nil {
			return err
		}
		if removed[r] {
			return invalid("removed period %s %s twice", r.Type, r.Key)
		}
		if periods[r] {
			// One calc_version of a period is either a snapshot or a removal (grpc-api.md 7.3).
			return invalid("period %s %s both sent and removed", r.Type, r.Key)
		}
		removed[r] = true
	}
	return nil
}

// IncompletePeriods returns the periods sent in one transfer mode only, in request order.
// They are applied as they came; the caller logs and counts them (grpc-api.md 7.3, 9).
func (u Upsert) IncompletePeriods() []PeriodKey {
	modes := map[PeriodKey]int{}
	var order []PeriodKey
	for _, s := range u.Snapshots {
		if modes[s.Key.PeriodKey] == 0 {
			order = append(order, s.Key.PeriodKey)
		}
		modes[s.Key.PeriodKey]++
	}
	var out []PeriodKey
	for _, k := range order {
		if modes[k] == 1 {
			out = append(out, k)
		}
	}
	return out
}

// Validate checks a snapshot and its metrics.
func (s *Snapshot) Validate() error {
	if err := s.Revisions.Validate(); err != nil {
		return err
	}
	if err := s.Key.Validate(); err != nil {
		return err
	}
	name := s.Key.Type.String() + " " + s.Key.Key
	if s.Key.Mode != TransferWith && s.Key.Mode != TransferWithout {
		return invalid("snapshot %s: transfer mode %d", name, s.Key.Mode)
	}
	// A snapshot is sent only for a period with covered days (grpc-api.md 7.3): its range,
	// data bounds and calculation moment are always there; empty ones belong to a marker.
	if s.Range == nil {
		return invalid("snapshot %s without range", name)
	}
	if err := s.Range.validate("range of snapshot " + name); err != nil {
		return err
	}
	if s.Coverage != CoverageComplete && s.Coverage != CoveragePartial {
		return invalid("snapshot %s: coverage %d", name, s.Coverage)
	}
	if s.DataFrom == nil || s.DataTo == nil || s.CalculatedAt == nil {
		return invalid("snapshot %s without data bounds or calculated_at", name)
	}
	if len(s.Gaps) > maxGaps {
		return invalid("snapshot %s: %d gaps", name, len(s.Gaps))
	}
	for _, g := range s.Gaps {
		if err := g.validate("gap of snapshot " + name); err != nil {
			return err
		}
	}
	ids := make(map[string]bool, len(s.Metrics))
	for i := range s.Metrics {
		m := &s.Metrics[i]
		if m.ID == "" {
			return invalid("snapshot %s: metric without id", name)
		}
		if ids[m.ID] {
			return invalid("snapshot %s: metric %s twice", name, m.ID)
		}
		ids[m.ID] = true
		if err := m.validate(); err != nil {
			return fmt.Errorf("snapshot %s: %w", name, err)
		}
	}
	return nil
}

// fitsSmallint is the column rule of an enum number kept as is: a newer producer's number
// is stored, one beyond smallint would be truncated into another value (65537 → READY).
func fitsSmallint(n int32) bool { return n >= math.MinInt16 && n <= math.MaxInt16 }

func (m *Metric) validate() error {
	if m.Status == statusUnspecified || m.Status == StatusUnavailable {
		return invalid("metric %s: status %d", m.ID, m.Status)
	}
	if !fitsSmallint(m.Status) {
		return invalid("metric %s: status %d out of smallint", m.ID, m.Status)
	}
	if m.Range != nil {
		if err := m.Range.validate("range of metric " + m.ID); err != nil {
			return err
		}
	}
	if l := m.Lock; l != nil {
		if l.Reason == 0 || !fitsSmallint(l.Reason) {
			return invalid("metric %s: lock reason %d", m.ID, l.Reason)
		}
	}
	if b := m.Basis; b != nil && b.Range != nil {
		if err := b.Range.validate("basis of metric " + m.ID); err != nil {
			return err
		}
	}
	if f := m.Filter; f != nil {
		// filter_from marks the filter: every transition of a metric has its period P.
		if f.From == nil || f.To == nil {
			return invalid("metric %s: filter without its period", m.ID)
		}
		if !fitsSmallint(f.Kind) || !fitsSmallint(f.TransferMode) {
			return invalid("metric %s: filter enum out of smallint", m.ID)
		}
	}
	return nil
}

// maxTimeZone bounds the IANA name kept in user_data_state.
const maxTimeZone = 64

// Validate checks UserDataState.
func (s *State) Validate() error {
	if err := s.Revisions.Validate(); err != nil {
		return err
	}
	if s.CalculatedAt == nil {
		return invalid("state without calculated_at")
	}
	if (s.DataFrom == nil) != (s.DataTo == nil) {
		return invalid("state with one data bound")
	}
	bounds := s.DataFrom != nil
	if bounds && !s.DataFrom.Before(*s.DataTo) {
		return invalid("state data bounds do not grow")
	}
	if (s.StaleAfter != nil) != bounds {
		return invalid("state stale_after without data bounds or the other way round")
	}
	if s.StaleAfter != nil && !s.StaleAfter.After(*s.DataTo) {
		return invalid("state stale_after not after data_to")
	}
	if s.HasData && (!bounds || s.LastOperationAt == nil) {
		return invalid("state has_data without bounds or last operation")
	}
	if s.FullMonths < 0 {
		return invalid("state full_months %d", s.FullMonths)
	}
	return ValidateTimeZone(s.TimeZone)
}

// ValidateTimeZone accepts an IANA zone name and nothing that merely loads: the empty
// name (UTC), "Local" (the server's zone), and paths with empty, "." or ".." segments
// are refused before time.LoadLocation, which resolves them.
func ValidateTimeZone(name string) error {
	if name == "" || len(name) > maxTimeZone || name == "Local" {
		return invalid("time zone %q", name)
	}
	for segment := range strings.SplitSeq(name, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return invalid("time zone %q", name)
		}
	}
	if _, err := time.LoadLocation(name); err != nil {
		return invalid("time zone %q", name)
	}
	return nil
}

// PeriodItem is a period with data: its key, calendar range and coverage.
type PeriodItem struct {
	Key        string
	Start, End time.Time
	Coverage   Coverage
}

// Periods is the answer of ListPeriods: periods of one type, newest first, and the zone
// they were calculated in (empty: no state yet).
type Periods struct {
	Revisions Revisions
	TimeZone  string
	Items     []PeriodItem
}
