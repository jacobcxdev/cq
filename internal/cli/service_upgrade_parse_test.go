package cli

import "testing"

func TestServiceUpgradeExecutablePathSyntax(t *testing.T) {
	for _, name := range []string{"candidate-executable", "service-executable"} {
		for _, test := range []struct {
			value string
			valid bool
		}{
			{"/opt/homebrew/bin/cq", true},
			{"/private/Runtime Candidate/cq", true},
			// File type and executable provenance are lifecycle preconditions.
			{"/", true},
			{"", false},
			{"cq", false},
			{"/private/./cq", false},
			{"/private/old/../cq", false},
			{"/private/cq/", false},
			{"/private/cq\x00", false},
		} {
			t.Run(name+"/"+test.value, func(t *testing.T) {
				value := nativeTestArgs([]string{test.value})[0]
				constraint := validateValue("service upgrade", ParameterSpec{Name: name, Type: "path"}, value, "")
				if (constraint == "") != test.valid {
					t.Fatalf("validateValue(%q, %q) = %q; valid=%t", name, value, constraint, test.valid)
				}
			})
		}
	}
}
