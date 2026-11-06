package dots

import (
	"strings"
	"testing"
)

func TestRspackBodyHandledLexicalDelimiters(t *testing.T) {
	for _, source := range []string{
		`{return /[{}]/.test("}")}`,
		"{const value=`outer${{key:`inner${1}`}}`;return value}",
		"{/* } */const value='{';// }\nreturn value}",
		`{return (10/2)+{value:1}.value}`,
	} {
		body, ok := rspackBody(source+";unrelated()", 0)
		if !ok || body != source {
			t.Fatalf("lexical delimiter mismatch: %q %v", body, ok)
		}
	}
}

func TestRspackBodyRejectedMalformedAndDeepTemplates(t *testing.T) {
	for _, source := range []string{"{", "{`unterminated", "{'unterminated}", "{/*unterminated}", "{return /unterminated}", "{`" + strings.Repeat("${{x:`", 130) + "text" + strings.Repeat("`}}", 130) + "`}"} {
		if _, ok := rspackBody(source, 0); ok {
			t.Fatalf("malformed body accepted: %.40q", source)
		}
	}
}
