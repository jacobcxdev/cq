package main

import "testing"

func TestZAICommandClassification(t *testing.T) {
	for _, test := range []struct {
		args []string
		row  string
	}{
		{[]string{"check", "zai"}, "check"},
		{[]string{"--json", "check", "codex", "zai"}, "check"},
		{[]string{"models", "list", "--provider", "zai"}, "models_list"},
		{[]string{"models", "overlay", "add", "--provider", "zai", "--id", "glm-future"}, "models_overlay_add"},
	} {
		authority, err := ClassifyProxyCommand(test.args)
		if err != nil || authority.Row != test.row {
			t.Errorf("ClassifyProxyCommand(%v) row = %q, err = %v; want %q", test.args, authority.Row, err, test.row)
		}
	}
}
