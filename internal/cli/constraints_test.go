package cli

import (
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

type clauseCoverage struct {
	ID             string   `json:"id"`
	Path           string   `json:"path"`
	Kind           string   `json:"parameter_kind"`
	Parameter      string   `json:"parameter"`
	Clause         string   `json:"clause"`
	Owner          string   `json:"semantic_owner"`
	Classification string   `json:"classification"`
	Cases          []string `json:"parser_cases"`
}

func TestCLIV2ConstraintCoverage(t *testing.T) {
	var coverage []clauseCoverage
	b, e := os.ReadFile("testdata/parser-coverage.json")
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(b, &coverage); e != nil {
		t.Fatal(e)
	}
	var ownership struct {
		Commands []struct {
			Path  string
			Owner string
		}
	}
	b, e = os.ReadFile("../../specs/cli-v2/plan/coverage.json")
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(b, &ownership); e != nil {
		t.Fatal(e)
	}
	owners := map[string]string{}
	for _, c := range ownership.Commands {
		owners[c.Path] = c.Owner
	}
	var spec struct {
		Commands []struct {
			ID, Path, Kind       string
			Options, Positionals []struct {
				Name       string
				Validation []string
			}
			Preconditions []string
		}
	}
	b, e = os.ReadFile("../../specs/cli-v2/commands.json")
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(b, &spec); e != nil {
		t.Fatal(e)
	}
	expected := map[string]string{}
	for _, c := range spec.Commands {
		if c.Kind != "command" {
			continue
		}
		for kind, ps := range map[string][]struct {
			Name       string
			Validation []string
		}{"options": c.Options, "positionals": c.Positionals} {
			for _, p := range ps {
				for i, clause := range p.Validation {
					expected[clauseID(c.ID, kind, p.Name, i)] = clause
				}
			}
		}
		for i, clause := range c.Preconditions {
			expected[clauseID(c.ID, "preconditions", "", i)] = clause
		}
	}
	cases := map[string]parseCase{}
	for _, c := range readParseCases(t, "testdata/parser-cases.json") {
		if _, ok := cases[c.ID]; ok {
			t.Fatalf("duplicate case %s", c.ID)
		}
		cases[c.ID] = c
	}
	seen := map[string]bool{}
	for _, c := range coverage {
		t.Run(c.ID, func(t *testing.T) {
			if seen[c.ID] {
				t.Fatal("duplicate coverage")
			}
			seen[c.ID] = true
			if expected[c.ID] != c.Clause {
				t.Fatalf("stale/unknown clause %s", c.Clause)
			}
			if c.ID == "codex.proxy.trace/options/session/1" && (c.Classification != "mixed" || c.Owner != "T16") {
				t.Error("trace selector syntax is parser-owned; privacy-key derivation must remain assigned to T16")
			}
			switch c.Classification {
			case "parser":
				if len(c.Cases) == 0 || c.Owner != "" {
					t.Fatal("parser clause needs executable cases only")
				}
			case "semantic":
				if c.Owner == "" || len(c.Cases) != 0 {
					t.Fatal("semantic clause needs exact owner")
				}
			case "mixed":
				if c.Owner == "" || len(c.Cases) == 0 {
					t.Fatal("mixed clause needs cases and owner")
				}
			default:
				t.Fatal("unclassified clause")
			}
			if c.Owner != "" && c.Owner != owners[c.Path] {
				t.Fatalf("invalid owner %s", c.Owner)
			}
			for _, id := range c.Cases {
				fixture, ok := cases[id]
				if !ok {
					t.Fatalf("missing executable fixture %s", id)
				}
				assertParseCase(t, fixture)
			}
		})
	}
	if len(seen) != len(expected) {
		t.Fatalf("coverage %d; catalogue %d", len(seen), len(expected))
	}
}
func clauseID(path, kind, name string, i int) string {
	s := path + "/" + kind + "/"
	if name != "" {
		s += name + "/"
	}
	return s + strconv.Itoa(i)
}
func TestCLIV2ConstraintConsentIsSemantic(t *testing.T) {
	for _, c := range catalogue {
		if c.Kind != "command" {
			continue
		}
		for _, o := range c.Options {
			if o.Type == "boolean" && o.Required {
				a := strings.Fields(c.Path)
				for _, required := range c.Options {
					if required.Required && required.Type != "boolean" {
						v := "/missing"
						if required.Type == "digest" {
							v = strings.Repeat("a", 64)
						}
						if required.Name == "client-build" {
							v = "1.2.3"
						}
						a = append(a, "--"+required.Name+"="+v)
					}
				}
				in, e := Parse(nativeTestArgs(a))
				if e != nil {
					t.Fatal(e)
				}
				if !reflect.DeepEqual(in.Options[o.Name], []string{"false"}) || in.Supplied[o.Name] {
					t.Errorf("consent default changed: %#v", in)
				}
			}
		}
	}
}
