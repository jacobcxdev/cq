package cli

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestTerminalQuantityRole(t *testing.T) {
	got := TerminalLine(TerminalOptions{Width: 100, Colour: true}, 0, TerminalCount(0))
	if !strings.Contains(got, "\x1b[1;34m") || strings.Contains(got, "\x1b[31m") {
		t.Fatalf("zero must be a blue quantity, not failure: %q", got)
	}
	if ansi.Strip(got) != "0\n" {
		t.Fatalf("visible output: %q", got)
	}
}

func TestTerminalLineEscapesHostileTextAndClosesStyles(t *testing.T) {
	got := TerminalLine(TerminalOptions{Width: 80, Colour: true}, 2,
		Span{Text: "name\x1b[31m\n\t", Role: Identity}, Span{Text: " failed", Role: Failed})
	want := "  \x1b[36mname\\u001b[31m\\u000a\\u0009\x1b[0m\x1b[31m failed\x1b[0m\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestTerminalLineWrapsGraphemeCells(t *testing.T) {
	for _, width := range []int{20, 40, 80, 120} {
		got := TerminalLine(TerminalOptions{Width: width}, 2, Span{Text: strings.Repeat("日本語👩‍💻 ", 18)})
		for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
			if ansi.StringWidth(line) > min(width, 100) {
				t.Fatalf("width %d: line exceeds body: %q", width, line)
			}
		}
	}
}

func TestTerminalTableFallsBackToOrderedRecords(t *testing.T) {
	got := TerminalTable(TerminalOptions{Width: 20}, []string{"Account", "Generation"}, [][]Span{{
		{Text: "long account name", Role: Identity}, {Text: "18446744073709551615", Role: Identity},
	}})
	want := "Account: long\naccount name\nGeneration:\n18446744073709551615\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestTerminalTableUsesColumnsWhenTheyFit(t *testing.T) {
	got := TerminalTable(TerminalOptions{Width: 40}, []string{"Name", "Count"}, [][]Span{{
		{Text: "alpha", Role: Identity}, TerminalCount(0),
	}})
	want := "Name   Count\nalpha  0\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestTerminalFieldsPreservesUnknownAndZero(t *testing.T) {
	got := TerminalFields(TerminalOptions{Width: 40}, 0, []Field{
		{Label: "Used", Value: []Span{TerminalCount(0)}},
		{Label: "Limit", Value: []Span{{Text: "—", Role: Secondary}}},
	})
	if got != "Used: 0\nLimit: —\n" {
		t.Fatalf("got %q", got)
	}
}
