package cli

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestCLIV2ConsentExactPromptAndOneResponse(t *testing.T) {
	for _, tc := range []struct {
		input string
		yes   bool
	}{
		{"y\n", true}, {"YES\n", true}, {" Yes \r\n", true}, {"\ty\t\n", true},
		{"\n", false}, {"no\n", false}, {"yeah\n", false}, {"yes please\n", false},
		{"", false}, {"yes", false}, {"y", false},
	} {
		t.Run(tc.input, func(t *testing.T) {
			var stderr, stdout bytes.Buffer
			prompt := "Remove local credentials for Research Ω? [y/N]"
			yes, err := Confirm(&Session{In: strings.NewReader(tc.input), Out: &stdout, Err: &stderr, Interactive: true}, prompt)
			if err != nil || yes != tc.yes || stderr.String() != prompt || stdout.Len() != 0 {
				t.Fatalf("consent: %t %v stdout=%q stderr=%q", yes, err, stdout.String(), stderr.String())
			}
		})
	}
	var stderr bytes.Buffer
	s := &Session{In: strings.NewReader("no\nyes\n"), Err: &stderr, Out: io.Discard, Interactive: true}
	first, e1 := Confirm(s, "First? [y/N]")
	second, e2 := Confirm(s, "Second? [y/N]")
	if e1 != nil || e2 != nil || first || !second || stderr.String() != "First? [y/N]Second? [y/N]" {
		t.Fatalf("read-ahead lost response: %t %t %v %v", first, second, e1, e2)
	}
}

type consentReadError struct{}

func (consentReadError) Read([]byte) (int, error) { return 0, errors.New("input failed") }

func TestCLIV2ConsentIOFailures(t *testing.T) {
	yes, err := Confirm(&Session{In: panicInput{}, Err: &failingOutput{}, Out: io.Discard, Interactive: true}, "Prompt")
	if yes || err == nil {
		t.Fatal("failed prompt read input or granted consent")
	}
	yes, err = Confirm(&Session{In: consentReadError{}, Err: io.Discard, Out: io.Discard, Interactive: true}, "Prompt")
	if yes || err == nil {
		t.Fatal("read failure granted consent")
	}
}
