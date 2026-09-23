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
	if got := TerminalCount(^uint64(0)); got.Text != "18446744073709551615" || got.Role != Quantity {
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
		{time.Date(2026, 3, 29, 0, 30, 0, 0, time.UTC), false, "2026-03-29 00:30:00 +00:00"},
		{time.Date(2026, 3, 29, 1, 30, 0, 0, time.UTC), false, "2026-03-29 02:30:00 +01:00"},
		{time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC), false, "2026-10-25 01:30:00 +01:00"},
		{time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC), false, "2026-10-25 01:30:00 +00:00"},
		{time.Date(2026, 9, 23, 15, 30, 0, 123456789, time.UTC), false, "2026-09-23 16:30:00.123456789 +01:00"},
		{time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), true, "2027-01-01 00:00:00 +00:00"},
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
	if got := TerminalTime(now.Add(-time.Second), options, true).Text; !strings.Contains(got, "past") {
		t.Fatalf("past deadline should be explicit: %q", got)
	}
}
