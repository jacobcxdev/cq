package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Check actual argv spellings and independent value assertions against each
// declared option. Adding unrelated fixtures cannot satisfy a missing spelling.
func TestCLIV2ValueSpellingCoverage(t *testing.T) {
	cases := readParseCases(t, "testdata/parser-cases.json")
	for _, command := range catalogue {
		if command.Kind != "command" {
			continue
		}
		for _, option := range command.Options {
			if option.Type == "boolean" {
				continue
			}
			for _, spelling := range []string{"equals", "separate"} {
				found := false
				for _, c := range cases {
					if c.Code == "" && c.Path == command.Path && len(c.Options[option.Name]) > 0 && valueSpelling(c.Args, "--"+option.Name, spelling) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("missing canonical %s --%s %s with an independent value expectation", command.Path, option.Name, spelling)
				}
			}
		}
	}
	var migration []struct {
		LegacyPath    string `json:"legacy_path"`
		CanonicalPath string `json:"canonical_path"`
		Disposition   string
		Variants      []struct{ Target string }
		Flags         []struct{ Legacy, Target string } `json:"flag_mappings"`
	}
	readSpellingJSON(t, "../../specs/cli-v2/migration.json", &migration)
	var rows []compatibilityRow
	readSpellingJSON(t, "testdata/compatibility.json", &rows)
	byPath := map[string][]compatibilityCase{}
	for _, row := range rows {
		byPath[row.LegacyPath] = row.Cases
	}
	for _, row := range migration {
		if row.Disposition == "frozen_machine_abi" {
			continue
		}
		paths := []string{row.CanonicalPath}
		for _, variant := range row.Variants {
			paths = append(paths, variant.Target)
		}
		for _, flag := range row.Flags {
			// These select the frozen T01 package-hook parser, never public Parse.
			if (row.LegacyPath == "service install" || row.LegacyPath == "service uninstall") && (flag.Legacy == "--owner" || flag.Legacy == "--installer-lock-held" || flag.Legacy == "--service-executable") {
				continue
			}
			if flag.Legacy == "--json" || flag.Legacy == "-j" || flag.Legacy == "--human" || flag.Legacy == "--clear" {
				continue
			}
			target := strings.TrimPrefix(strings.Split(flag.Target, " ")[0], "--")
			positional, consumed, retired := false, false, row.Disposition == "retired"
			if flag.Target == "positional operation-id" {
				target, positional = "operation-id", true
			}
			if row.LegacyPath == "proxy candidate artifact switch" && flag.Legacy == "--role" {
				target, consumed = "role", true
			}
			if retired {
				target = strings.TrimPrefix(flag.Legacy, "--")
			}
			valueOption := positional || consumed || retired
			declared := valueOption
			for _, command := range catalogue {
				for _, path := range paths {
					if command.Path != path {
						continue
					}
					for _, option := range command.Options {
						if option.Name == target {
							declared = true
							valueOption = option.Type != "boolean"
						}
					}
				}
			}
			if !declared {
				t.Errorf("unclassified legacy mapping %s %s -> %s", row.LegacyPath, flag.Legacy, flag.Target)
				continue
			}
			if !valueOption {
				continue
			}
			for _, spelling := range []string{"equals", "separate"} {
				found := false
				for _, c := range byPath[row.LegacyPath] {
					if c.Reject || c.Path == "" || !valueSpelling(c.Legacy, flag.Legacy, spelling) {
						continue
					}
					independent := len(c.Options[target]) > 0
					if positional {
						independent = len(c.Arguments[target]) > 0
					}
					if consumed {
						for _, absent := range c.AbsentOptions {
							if absent == target {
								independent = true
							}
						}
					}
					if retired {
						independent = c.Code == "operation_recovery_unavailable" && c.Exit == 4
					}
					if independent && (c.Code == "" || retired) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("missing legacy %s %s %s with an independent value expectation", row.LegacyPath, flag.Legacy, spelling)
				}
			}
		}
	}
}

func readSpellingJSON(t *testing.T, path string, target any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, target); err != nil {
		t.Fatal(err)
	}
}
func valueSpelling(args []string, option, spelling string) bool {
	for i, arg := range args {
		if spelling == "equals" && strings.HasPrefix(arg, option+"=") {
			return true
		}
		if spelling == "separate" && arg == option && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			return true
		}
	}
	return false
}

func TestCLIV2ValueInterspersedPositionals(t *testing.T) {
	for _, c := range []parseCase{
		{ID: "canonical-repeatable", Args: []string{"check", "claude", "--timeout", "5s", "codex"}, Path: "check", Options: map[string][]string{"timeout": {"5s"}}, Arguments: map[string][]string{"providers": {"claude", "codex"}}},
		{ID: "canonical-before-positional", Args: []string{"codex", "account", "remove", "--timeout", "5s", "account"}, Path: "codex account remove", Options: map[string][]string{"timeout": {"5s"}}, Arguments: map[string][]string{"account": {"account"}}},
		{ID: "alias-duration-before-option", Args: []string{"proxy", "status", "7s", "--port", "29280"}, Path: "proxy health", Options: map[string][]string{"timeout": {"7s"}, "port": {"29280"}}},
		{ID: "alias-positional-before-option", Args: []string{"codex", "remove", "account", "--timeout", "5s"}, Path: "codex account remove", Options: map[string][]string{"timeout": {"5s"}}, Arguments: map[string][]string{"account": {"account"}}},
	} {
		t.Run(c.ID, func(t *testing.T) { assertParseCase(t, c) })
	}
}
