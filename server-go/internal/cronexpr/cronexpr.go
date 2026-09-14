// Package cronexpr evaluates a five-field cron expression -- `minute hour day-of-month
// month day-of-week` -- in UTC.
//
// Written here rather than taken from a dependency because the whole need is "when is the
// next run", the syntax is the standard subset every operator already knows, and adding a
// dependency for sixty lines is not a trade worth making.
//
// Supported per field: `*`, `N`, `A-B`, `*/S`, `A-B/S`, comma lists of those, and the usual
// three-letter names for months (jan..dec) and weekdays (sun..sat). Day-of-week accepts 0-7
// with both 0 and 7 meaning Sunday. Day-of-month and day-of-week combine with OR when both
// are restricted, as in Vixie cron.
package cronexpr

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const DefaultSchedule = "*/30 * * * *"

var months = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

var days = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

type fieldSpec struct {
	lo, hi int
	names  map[string]int
}

// In expression order.
var fields = []fieldSpec{
	{0, 59, nil},
	{0, 23, nil},
	{1, 31, nil},
	{1, 12, months},
	{0, 7, days},
}

// Error reports an expression this package does not understand.
type Error struct{ msg string }

func (e *Error) Error() string { return e.msg }

func errf(format string, args ...any) error {
	return &Error{msg: fmt.Sprintf(format, args...)}
}

// IsCronError reports whether err came from parsing or walking a schedule.
func IsCronError(err error) bool {
	var target *Error
	return errors.As(err, &target)
}

// Schedule is a parsed expression plus whether day-of-month / day-of-week were restricted,
// which the Vixie OR rule needs.
type Schedule struct {
	Minute, Hour, Dom, Month, Dow map[int]bool
	DomSet, DowSet                bool
}

func atom(text string, spec fieldSpec) (int, error) {
	key := strings.ToLower(strings.TrimSpace(text))
	if spec.names != nil {
		if v, ok := spec.names[key]; ok {
			return v, nil
		}
	}
	value, err := strconv.Atoi(key)
	if err != nil {
		return 0, errf("%q is not a number", text)
	}
	if value < spec.lo || value > spec.hi {
		return 0, errf("%d is outside %d-%d", value, spec.lo, spec.hi)
	}
	return value, nil
}

func parseField(text string, spec fieldSpec) (map[int]bool, error) {
	values := map[int]bool{}
	for _, part := range strings.Split(text, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, errf("empty list item")
		}
		step := 1
		if idx := strings.Index(part, "/"); idx >= 0 {
			stepText := part[idx+1:]
			part = part[:idx]
			n, err := strconv.Atoi(stepText)
			if err != nil || n < 1 {
				return nil, errf("bad step %q", stepText)
			}
			step = n
		}
		var start, end int
		switch {
		case part == "*":
			start, end = spec.lo, spec.hi
		case strings.Contains(part, "-"):
			bits := strings.SplitN(part, "-", 2)
			a, err := atom(bits[0], spec)
			if err != nil {
				return nil, err
			}
			b, err := atom(bits[1], spec)
			if err != nil {
				return nil, err
			}
			if a > b {
				return nil, errf("range %q runs backwards", part)
			}
			start, end = a, b
		default:
			v, err := atom(part, spec)
			if err != nil {
				return nil, err
			}
			// `N/S` means "from N to the end, every S"; a bare N is just N.
			start = v
			end = v
			if step > 1 {
				end = spec.hi
			}
		}
		for v := start; v <= end; v += step {
			values[v] = true
		}
	}
	return values, nil
}

// Parse reads an expression into a Schedule.
func Parse(expr string) (*Schedule, error) {
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return nil, errf("expected 5 fields: minute hour day month weekday")
	}
	sets := make([]map[int]bool, 5)
	for i, text := range parts {
		set, err := parseField(text, fields[i])
		if err != nil {
			return nil, err
		}
		sets[i] = set
	}
	dow := sets[4]
	if dow[7] {
		delete(dow, 7)
		dow[0] = true
	}
	return &Schedule{
		Minute: sets[0], Hour: sets[1], Dom: sets[2], Month: sets[3], Dow: dow,
		DomSet: parts[2] != "*", DowSet: parts[4] != "*",
	}, nil
}

// Validate returns an empty string when the expression is valid, else the reason.
func Validate(expr string) string {
	if _, err := Parse(expr); err != nil {
		return err.Error()
	}
	return ""
}

// NextAfter returns the first matching instant strictly after `after` (UTC, seconds dropped).
//
// Walks forward in the coarsest unit that fails to match, so a yearly schedule is found in a
// few hundred steps rather than half a million. Bounded at five years: a schedule that never
// fires (Feb 30) is reported rather than looped on.
func NextAfter(expr string, after time.Time) (time.Time, error) {
	schedule, err := Parse(expr)
	if err != nil {
		return time.Time{}, err
	}
	t := after.UTC().Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(0, 0, 366*5)
	for t.Before(limit) {
		if !schedule.Month[int(t.Month())] {
			t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
			continue
		}
		if !dayMatches(t, schedule) {
			t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
			continue
		}
		if !schedule.Hour[t.Hour()] {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, time.UTC).Add(time.Hour)
			continue
		}
		if !schedule.Minute[t.Minute()] {
			t = t.Add(time.Minute)
			continue
		}
		return t, nil
	}
	return time.Time{}, errf("schedule never fires")
}

func dayMatches(t time.Time, s *Schedule) bool {
	inDom, inDow := s.Dom[t.Day()], s.Dow[int(t.Weekday())]
	if s.DomSet && s.DowSet {
		return inDom || inDow
	}
	return inDom && inDow
}
