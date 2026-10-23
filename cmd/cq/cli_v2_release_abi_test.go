package main

import "testing"

func TestCLIV2ReleasedUpgradeABI(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"service", "upgrade", "--owner=homebrew", "--candidate-executable=/tmp/cq"}, true},
		{[]string{"service", "upgrade", "--owner=homebrew", "--candidate-executable=/tmp/cq", "--json"}, true},
		{[]string{"service", "upgrade", "--owner=manual", "--candidate-executable=/tmp/cq"}, false},
		{[]string{"service", "upgrade", "--owner=homebrew", "--candidate-executable=relative"}, false},
		{[]string{"service", "install", "--owner=homebrew", "--candidate-executable="}, false},
		{[]string{"service", "install", "--owner=homebrew", "--snapshot-file="}, false},
		{[]string{"service", "install", "--owner=homebrew", "--candidate-executable", ""}, false},
		{[]string{"service", "install", "--owner=homebrew", "--service-executable", ""}, false},
		{[]string{"service", "install", "--owner=homebrew", "--snapshot-file", ""}, false},
		{[]string{"proxy", "runtime-check", "--json"}, true},
		{[]string{"proxy", "runtime-check", "--json", "extra"}, false},
		{[]string{"proxy", "runtime-check", "--help"}, false},
	} {
		if got := isMachineABI(tc.args); got != tc.want {
			t.Errorf("isMachineABI(%q)=%t want %t", tc.args, got, tc.want)
		}
	}
}
