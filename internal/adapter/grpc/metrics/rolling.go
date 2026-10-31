package metrics

import (
	"errors"
	"strconv"
	"time"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"

	"github.com/fin-assist/analytics/internal/domain"
)

// rollingPoint refuses a malformed producer slice rather than widening a financial point.
func rollingPoint(point *analyticsv1.MonthlyPoint, period domain.TimeRange, zone *time.Location) error {
	calendar, slice := point.GetRange(), point.GetCalculationRange()
	if (domain.PeriodKey{Type: domain.PeriodMonth, Key: point.GetMonth()}).Validate() != nil || slice.GetStart() == nil || slice.GetEnd() == nil || calendar.GetStart() == nil || calendar.GetEnd() == nil {
		return errors.New("rolling point range absent")
	}
	if slice.Start.CheckValid() != nil || slice.End.CheckValid() != nil || calendar.Start.CheckValid() != nil || calendar.End.CheckValid() != nil {
		return errors.New("rolling point invalid timestamp")
	}
	start, end := slice.Start.AsTime(), slice.End.AsTime()
	fullStart, fullEnd := calendar.Start.AsTime(), calendar.End.AsTime()
	if !rollingMonthMatches(point.Month, fullStart, fullEnd, zone) {
		return errors.New("rolling point calendar identity mismatch")
	}
	expectedStart, expectedEnd := calendar.Start.AsTime(), calendar.End.AsTime()
	if period.Start.After(expectedStart) {
		expectedStart = *period.Start
	}
	if period.End.Before(expectedEnd) {
		expectedEnd = *period.End
	}
	if !start.Before(end) || !start.Equal(expectedStart) || !end.Equal(expectedEnd) {
		return errors.New("rolling point outside calculation range")
	}
	switch point.GetBounds().GetCoverage() {
	case commonv1.Coverage_COVERAGE_NO_DATA:
		if point.ExpenseMinor != nil || point.IncomeMinor != nil {
			return errors.New("uncovered rolling point has amounts")
		}
	case commonv1.Coverage_COVERAGE_COMPLETE:
		if !start.Equal(fullStart) || !end.Equal(fullEnd) {
			return errors.New("rolling edge claims a complete calendar month")
		}
		fallthrough
	case commonv1.Coverage_COVERAGE_PARTIAL:
		if point.ExpenseMinor == nil || point.IncomeMinor == nil {
			return errors.New("covered rolling point has no amounts")
		}
	default:
		return errors.New("rolling point unknown coverage")
	}
	return nil
}

// rollingMonthMatches uses real civil boundaries for the new point format, including midnight transitions.
func rollingMonthMatches(key string, start, end time.Time, zone *time.Location) bool {
	if (domain.PeriodKey{Type: domain.PeriodMonth, Key: key}).Validate() != nil {
		return false
	}
	year, _ := strconv.Atoi(key[:4])
	month, _ := strconv.Atoi(key[5:])
	first, exists := domain.CivilBoundary(year, time.Month(month), 1, zone)
	if !exists {
		return false
	}
	next := time.Date(year, time.Month(month)+1, 1, 12, 0, 0, 0, time.UTC)
	last, exists := domain.CivilBoundary(next.Year(), next.Month(), 1, zone)
	return exists && first.Equal(start) && last.Equal(end)
}
