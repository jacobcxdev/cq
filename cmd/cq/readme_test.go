package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestREADMEListsEveryPublicCommandPath(t *testing.T) {
	raw, err := os.ReadFile("../../specs/cli-v2/commands.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct{ Commands []struct{ Path string } }
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	want := make([]string, 0, len(spec.Commands))
	for _, command := range spec.Commands {
		want = append(want, "cq "+command.Path)
	}
	if err := validateREADMECommandIndex(string(raw), want); err != nil {
		t.Fatal(err)
	}
}

func validateREADMECommandIndex(readme string, want []string) error {
	const start = "<!-- public-command-index:start -->"
	const end = "<!-- public-command-index:end -->"
	if strings.Count(readme, start) != 1 || strings.Count(readme, end) != 1 {
		return fmt.Errorf("README requires one marked command index")
	}
	_, after, _ := strings.Cut(readme, start)
	index, _, found := strings.Cut(after, end)
	if !found {
		return fmt.Errorf("README command index markers out of order")
	}
	seen := map[string]bool{}
	var got []string
	for _, line := range strings.Split(index, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "```text" || line == "```" {
			continue
		}
		if !strings.HasPrefix(line, "cq ") {
			return fmt.Errorf("unexpected command index line %q", line)
		}
		if seen[line] {
			return fmt.Errorf("duplicate command index entry %q", line)
		}
		seen[line] = true
		got = append(got, line)
	}
	expected := append([]string(nil), want...)
	sort.Strings(expected)
	sort.Strings(got)
	if strings.Join(got, "\n") != strings.Join(expected, "\n") {
		return fmt.Errorf("README command index differs: got %q; want %q", got, expected)
	}
	return nil
}

func TestREADMECommandIndexRejectsDrift(t *testing.T) {
	const prefix = "Example outside index: cq check\n<!-- public-command-index:start -->\n```text\n"
	const suffix = "```\n<!-- public-command-index:end -->\n"
	for _, tc := range []struct {
		name, entries string
		valid         bool
	}{
		{"exact", "cq check\ncq service status\n", true},
		{"missing-but-in-example", "cq service status\n", false},
		{"obsolete", "cq check\ncq service status\ncq obsolete\n", false},
		{"duplicate", "cq check\ncq check\ncq service status\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateREADMECommandIndex(prefix+tc.entries+suffix, []string{"cq check", "cq service status"})
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%t err=%v", tc.valid, err)
			}
		})
	}
}

func TestREADMEDocumentsCompleteInstallationParity(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	readme := string(body)
	normalised := strings.Join(strings.Fields(readme), " ")
	for _, required := range []string{
		"brew install --cask jacobcxdev/tap/cq",
		"winget install jacobcxdev.cq",
		"go run github.com/jacobcxdev/cq/cmd/cq-install@latest",
		"No manual post-install command is required",
		"brew services stop cq",
		"brew uninstall --formula cq",
		"brew uninstall --cask cq",
		"winget uninstall jacobcxdev.cq",
		"go run github.com/jacobcxdev/cq/cmd/cq-install@latest uninstall",
		"configuration, credentials, cache, history, and logs remain",
		"functional systemd user manager",
		"current Windows user without administrator access",
		"## Development and portable binaries",
		"go install github.com/jacobcxdev/cq/cmd/cq@latest",
		"does not install or manage services",
		"headroom-ai",
	} {
		if !strings.Contains(normalised, required) {
			t.Errorf("README missing installation contract %q", required)
		}
	}
	for _, obsolete := range []string{
		"brew install jacobcxdev/tap/cq\n",
		"brew services start cq            # Optional local proxy service",
	} {
		if strings.Contains(readme, obsolete) {
			t.Errorf("README retains obsolete installation instruction %q", obsolete)
		}
	}
}
