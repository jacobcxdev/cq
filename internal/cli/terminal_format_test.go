package cli

import (
	"strings"
	"testing"
	"time"
)

func TestTerminalPercentKeepsExactSettingsAndBoundsObservations(t *testing.T) {
	for _, tc := range []struct {
		value float64
		exact bool
		want  string
	}{
		{2.05, true, "2.05%"},
		{0.04, false, "<0.1%"},
		{99.96, false, ">99.9%"},
		{0, false, "0%"},
		{100, false, "100%"},
	} {
		if got := TerminalPercent(tc.value, tc.exact); got.Text != tc.want || got.Role != Quantity {
			t.Fatalf("percent(%v, %v) = %#v, want %q quantity", tc.value, tc.exact, got, tc.want)
		}
	}
}

func TestTerminalCountDoesNotNarrowUint64(t *testing.T) {
	if got := TerminalCount(^uint64(0)); got.Text != "18,446,744,073,709,551,615" || got.Role != Quantity {
		t.Fatalf("max count: %#v", got)
	}
}

func TestTerminalTimeKeepsYearOffsetAndPrecision(t *testing.T) {
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	options := TerminalOptions{Now: time.Date(2026, 9, 23, 15, 30, 0, 0, time.UTC), Location: london}
	for _, tc := range []struct {
		when     time.Time
		deadline bool
		want     string
	}{
		{time.Date(2026, 3, 29, 0, 30, 0, 0, time.UTC), false, "29 Mar 2026, 00:30 GMT (UTC+00:00)"},
		{time.Date(2026, 3, 29, 1, 30, 0, 0, time.UTC), false, "29 Mar 2026, 02:30 BST (UTC+01:00)"},
		{time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC), false, "25 Oct 2026, 01:30 BST (UTC+01:00)"},
		{time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC), false, "25 Oct 2026, 01:30 GMT (UTC+00:00)"},
		{time.Date(2026, 9, 23, 15, 30, 0, 123456789, time.UTC), false, "23 Sep 2026, 16:30:00.123456789 BST (UTC+01:00)"},
		{time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), true, "1 Jan 2027, 00:00 GMT (UTC+00:00)"},
	} {
		got := TerminalTime(tc.when, options, tc.deadline)
		if !strings.Contains(got.Text, tc.want) || got.Role != TimeValue {
			t.Fatalf("time(%s) = %#v, want %s", tc.when, got, tc.want)
		}
	}
}

func TestTerminalTimeRoundsRemainingUpAndAgeDown(t *testing.T) {
	now := time.Date(2026, 9, 23, 15, 30, 0, 0, time.UTC)
	options := TerminalOptions{Now: now, Location: time.UTC}
	if got := TerminalTime(now.Add(61*time.Second), options, true).Text; !strings.Contains(got, "in 2m") {
		t.Fatalf("remaining should round up: %q", got)
	}
	if got := TerminalTime(now.Add(-61*time.Second), options, false).Text; !strings.Contains(got, "1m ago") {
		t.Fatalf("age should round down: %q", got)
	}
	if got := TerminalTime(now.Add(-time.Second), options, true).Text; !strings.Contains(got, "expired 1s ago") {
		t.Fatalf("past deadline should be explicit: %q", got)
	}
	if got := TerminalTime(now.Add(24*time.Hour+time.Second), options, true).Text; !strings.Contains(got, "in 1d 1h") {
		t.Fatalf("remaining days should round the displayed hour up: %q", got)
	}
	if got := TerminalTime(now.Add(-24*time.Hour-time.Second), options, false).Text; !strings.Contains(got, "1d ago") {
		t.Fatalf("elapsed days should round the displayed hour down: %q", got)
	}
}

func TestTerminalCountGrouping(t *testing.T) {
	if got := TerminalCount(1000); got.Text != "1,000" || got.Role != Quantity {
		t.Fatal(got)
	}
}
func TestTerminalTimeRelativeFirst(t *testing.T) {
	now := time.Date(2026, 9, 23, 15, 30, 0, 0, time.UTC)
	o := TerminalOptions{Now: now, Location: time.UTC}
	for _, tc := range []struct {
		delta time.Duration
		want  string
	}{{2 * time.Hour, "in 2h · "}, {48 * time.Hour, "in 2d · "}, {-48 * time.Hour, "2d ago · "}} {
		if got := TerminalTime(now.Add(tc.delta), o, false); !strings.HasPrefix(got.Text, tc.want) {
			t.Fatal(got)
		}
	}
}

func TestTerminalTimestampAuditPrecision(t *testing.T) {
	london, _ := time.LoadLocation("Europe/London")
	for _, tc := range []struct{ when, want string }{
		{"2026-09-23T15:30:00Z", "23 Sep 2026, 16:30:00 BST (UTC+01:00)"},
		{"2026-10-25T01:30:00.123456789Z", "25 Oct 2026, 01:30:00.123456789 GMT (UTC+00:00)"},
	} {
		when, _ := time.Parse(time.RFC3339Nano, tc.when)
		got := TerminalTimestamp(when, TerminalOptions{Location: london})
		if got.Text != tc.want || got.Role != TimeValue {
			t.Fatal(got)
		}
	}
}
func TestTerminalTimeSmallIntervalsAndNow(t *testing.T) {
	now := time.Date(2026, 9, 23, 15, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		delta time.Duration
		want  string
	}{{0, "now · "}, {501 * time.Microsecond, "in 1ms · "}, {1500 * time.Millisecond, "in 2s · "}, {-1500 * time.Millisecond, "expired 1s ago · "}} {
		got := TerminalTime(now.Add(tc.delta), TerminalOptions{Now: now, Location: time.UTC}, true)
		if !strings.HasPrefix(got.Text, tc.want) {
			t.Fatal(got)
		}
	}
}
