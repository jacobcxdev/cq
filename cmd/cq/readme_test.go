package main

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	for _, command := range spec.Commands {
		if !strings.Contains(string(raw), "cq "+command.Path) {
			t.Errorf("README missing %s", command.Path)
		}
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
