package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// terminalFixtureRegistration records the independent authored contract, including
// stream identity where a scenario has both a receipt and diagnostics.
type terminalFixtureRegistration struct{ Command, Scenario, Stream, File string }

func assertTerminalFixture(t *testing.T, relative string, got string) {
	t.Helper()
	path := filepath.Join("..", "..", "specs", "cli-v2", "presentation", "examples", relative)
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if ansi.Strip(got) != string(want) {
		t.Fatalf("terminal fixture %s differs\nwant:\n%s\ngot:\n%s", relative, want, ansi.Strip(got))
	}
}

func TestTerminalCatalogueManifestCoverage(t *testing.T) {
	base := filepath.Join("..", "..")
	var manifest struct {
		Commands []json.RawMessage `json:"commands"`
		Groups   struct {
			Task  string
			Paths []string
		} `json:"groups"`
		Fixtures []struct{ Command, Scenario, Stream, File, Task string }
	}
	manifestBytes, err := os.ReadFile(filepath.Join(base, "docs", "superpowers", "plans", "terminal-presentation-coverage.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	var index struct {
		HelpPages int `json:"help_pages"`
		Fixtures  []struct {
			Command, Scenario, Stream, File, SHA256 string
		}
	}
	examples := filepath.Join(base, "specs", "cli-v2", "presentation", "examples")
	indexBytes, err := os.ReadFile(filepath.Join(examples, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(indexBytes, &index); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Commands) != 99 || manifest.Groups.Task != "T03" || len(manifest.Groups.Paths) != 39 || index.HelpPages != 138 || len(index.Fixtures) != 560 || len(manifest.Fixtures) != len(index.Fixtures) {
		t.Fatalf("catalogue size changed: commands=%d groups-and-root=%d helps=%d fixtures=%d assigned=%d", len(manifest.Commands), len(manifest.Groups.Paths), index.HelpPages, len(index.Fixtures), len(manifest.Fixtures))
	}
	var catalogue struct {
		Commands []struct{ Path, Kind string }
	}
	catalogueBytes, err := os.ReadFile(filepath.Join(base, "specs", "cli-v2", "commands.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(catalogueBytes, &catalogue); err != nil {
		t.Fatal(err)
	}
	commandPaths := make(map[string]bool)
	groupPaths := map[string]bool{"cq": true}
	for _, entry := range catalogue.Commands {
		path := "cq " + entry.Path
		switch entry.Kind {
		case "command":
			if commandPaths[path] {
				t.Errorf("duplicate catalogue command %s", path)
			}
			commandPaths[path] = true
		case "group":
			if groupPaths[path] {
				t.Errorf("duplicate catalogue group %s", path)
			}
			groupPaths[path] = true
		default:
			t.Errorf("unexpected catalogue kind %q", entry.Kind)
		}
	}
	assignedCommands := make(map[string]bool)
	for _, encoded := range manifest.Commands {
		var row struct{ Command string }
		if err := json.Unmarshal(encoded, &row); err != nil {
			t.Fatal(err)
		}
		if assignedCommands[row.Command] || !commandPaths[row.Command] {
			t.Errorf("duplicate or unknown assigned command %s", row.Command)
		}
		assignedCommands[row.Command] = true
	}
	assignedGroups := make(map[string]bool)
	for _, path := range manifest.Groups.Paths {
		if assignedGroups[path] || !groupPaths[path] {
			t.Errorf("duplicate or unknown assigned group %s", path)
		}
		assignedGroups[path] = true
	}
	if len(assignedCommands) != len(commandPaths) || len(assignedGroups) != len(groupPaths) {
		t.Errorf("catalogue paths missing: commands %d/%d groups %d/%d", len(assignedCommands), len(commandPaths), len(assignedGroups), len(groupPaths))
	}
	registered := make(map[string]terminalFixtureRegistration, len(index.Fixtures))
	add := func(f terminalFixtureRegistration) {
		if _, exists := registered[f.File]; exists {
			t.Errorf("fixture registered twice: %s", f.File)
		}
		registered[f.File] = f
	}
	for _, f := range terminalAccountsFixtures {
		add(f)
	}
	for _, f := range terminalResetsFixtures {
		if f.Command != "cq codex reset" { // Group syntax is owned by T03.
			add(f)
		}
	}
	for _, f := range terminalDotsFixtures {
		add(f)
	}
	for _, f := range terminalModelsFixtures {
		add(f)
	}
	for _, f := range terminalRoutingFixtures {
		add(f.terminalFixtureRegistration)
	}
	for _, f := range terminalValidationFixtures {
		add(f)
	}
	for _, f := range terminalEndpointFixtures {
		add(f)
	}
	for _, f := range terminalServiceFixtures {
		add(f)
	}
	for _, f := range terminalCheckFixtures {
		add(f)
	}
	for _, f := range terminalCandidateFixtures {
		add(f)
	}
	for _, f := range terminalProtocolFixtures {
		add(f)
	}
	assigned := make(map[string]terminalFixtureRegistration, len(manifest.Fixtures))
	for _, row := range manifest.Fixtures {
		f := terminalFixtureRegistration{row.Command, row.Scenario, row.Stream, row.File}
		if _, exists := assigned[row.File]; exists {
			t.Errorf("fixture assigned twice: %s", row.File)
		}
		assigned[row.File] = f
		// T03 tests every help, parse-error, version and completion fixture.
		// T10's runtime test walks every owned row and fails on unknown files.
		if row.Task == "T03" || row.Task == "T10" {
			add(f)
		}
	}
	if len(registered) != len(index.Fixtures) {
		t.Errorf("registered %d of %d fixtures", len(registered), len(index.Fixtures))
	}
	indexed := make(map[string]bool, len(index.Fixtures))
	for _, row := range index.Fixtures {
		if indexed[row.File] {
			t.Errorf("indexed twice: %s", row.File)
		}
		indexed[row.File] = true
		f := terminalFixtureRegistration{row.Command, row.Scenario, row.Stream, row.File}
		if registered[row.File] != f || assigned[row.File] != f {
			t.Errorf("registration mismatch: %s", row.File)
		}
		data, err := os.ReadFile(filepath.Join(examples, row.File))
		if err != nil {
			t.Errorf("read fixture %s: %v", row.File, err)
			continue
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != row.SHA256 {
			t.Errorf("fixture hash changed: %s", row.File)
		}
	}
}
