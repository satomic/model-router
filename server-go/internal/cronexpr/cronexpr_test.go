package cronexpr

import (
	"testing"
	"time"
)

func TestNextAfter(t *testing.T) {
	from := time.Date(2026, 3, 10, 14, 7, 30, 0, time.UTC)
	cases := []struct {
		expr string
		want string
	}{
		{"*/30 * * * *", "2026-03-10T14:30:00Z"},
		{"0 * * * *", "2026-03-10T15:00:00Z"},
		{"0 3 * * *", "2026-03-11T03:00:00Z"},
		{"15,45 * * * *", "2026-03-10T14:15:00Z"},
		{"0 0 1 * *", "2026-04-01T00:00:00Z"},
		{"0 9 * * mon", "2026-03-16T09:00:00Z"},
		{"0 0 1 jan *", "2027-01-01T00:00:00Z"},
		{"0 9-17/4 * * *", "2026-03-10T17:00:00Z"},
		// Vixie's rule: when both day-of-month and day-of-week are restricted they combine with
		// OR, so the 13th matches even though it is not a Monday.
		{"0 0 13 * mon", "2026-03-13T00:00:00Z"},
	}
	for _, c := range cases {
		got, err := NextAfter(c.expr, from)
		if err != nil {
			t.Errorf("%s: %v", c.expr, err)
			continue
		}
		if got.Format(time.RFC3339) != c.want {
			t.Errorf("%s: next = %s, want %s", c.expr, got.Format(time.RFC3339), c.want)
		}
	}
}

func TestNextAfterIsStrictlyAfter(t *testing.T) {
	// A schedule that has just fired must report the *next* firing, not the one that already ran.
	from := time.Date(2026, 3, 10, 14, 30, 0, 0, time.UTC)
	got, err := NextAfter("*/30 * * * *", from)
	if err != nil {
		t.Fatal(err)
	}
	if !got.After(from) {
		t.Errorf("next = %s, want strictly after %s", got, from)
	}
}

func TestValidateRejectsNonsense(t *testing.T) {
	for _, expr := range []string{"", "* * * *", "* * * * * *", "60 * * * *", "bogus", "*/0 * * * *", "5-1 * * * *"} {
		if problem := Validate(expr); problem == "" {
			t.Errorf("%q was accepted, want a reason", expr)
		}
	}
	for _, expr := range []string{"* * * * *", "*/30 * * * *", "0 9 * * mon-fri", "0 0 1 jan,jul *"} {
		if problem := Validate(expr); problem != "" {
			t.Errorf("%q was rejected: %s", expr, problem)
		}
	}
}

func TestSundayIsBothZeroAndSeven(t *testing.T) {
	from := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC) // a Tuesday
	zero, err := NextAfter("0 0 * * 0", from)
	if err != nil {
		t.Fatal(err)
	}
	seven, err := NextAfter("0 0 * * 7", from)
	if err != nil {
		t.Fatal(err)
	}
	if !zero.Equal(seven) {
		t.Errorf("0 gave %s and 7 gave %s; both spell Sunday", zero, seven)
	}
	if zero.Weekday() != time.Sunday {
		t.Errorf("landed on %s, want Sunday", zero.Weekday())
	}
}

func TestScheduleThatNeverFires(t *testing.T) {
	// February 30th: reported rather than looped on.
	if _, err := NextAfter("0 0 30 feb *", time.Now()); err == nil {
		t.Error("a schedule that never fires should be reported")
	}
}
