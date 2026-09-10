package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCLIV2GeneratedHelp(t *testing.T) {
	want, err := os.ReadFile("../../specs/cli-v2/help/codex-proxy-reserve-set.txt")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := Help("codex proxy reserve set")
	if !ok || got != string(want) {
		t.Fatal("help differs from canonical specification")
	}
}

func TestCLIV2GeneratedCatalogue(t *testing.T) {
	type rawParameter struct {
		Name       string `json:"name"`
		Type       string `json:"type"`
		Short      string `json:"short"`
		Choices    []string
		Default    any
		Required   bool
		Repeatable bool
	}
	type rawCommand struct {
		Path        string
		Kind        string
		Options     []rawParameter
		Positionals []rawParameter
	}
	var spec struct {
		GlobalOptions []rawParameter `json:"global_options"`
		Commands      []rawCommand
	}
	content, err := os.ReadFile("../../specs/cli-v2/commands.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(content, &spec); err != nil {
		t.Fatal(err)
	}
	convertParameter := func(raw rawParameter) ParameterSpec {
		choices := raw.Choices
		if choices == nil {
			choices = []string{}
		}
		var defaults []string
		switch value := raw.Default.(type) {
		case nil:
		case []any:
			defaults = make([]string, 0, len(value))
			for _, item := range value {
				defaults = append(defaults, fmt.Sprint(item))
			}
		default:
			defaults = []string{fmt.Sprint(value)}
		}
		return ParameterSpec{Name: raw.Name, Type: raw.Type, Short: raw.Short, Choices: choices, Default: defaults, Required: raw.Required, Repeatable: raw.Repeatable}
	}
	convertParameters := func(raw []rawParameter) []ParameterSpec {
		result := make([]ParameterSpec, 0, len(raw))
		for _, parameter := range raw {
			result = append(result, convertParameter(parameter))
		}
		return result
	}
	wantGlobals := convertParameters(spec.GlobalOptions)
	if !reflect.DeepEqual(globalOptions, wantGlobals) {
		t.Fatalf("global options differ:\n got %#v\nwant %#v", globalOptions, wantGlobals)
	}
	if len(catalogue) != len(spec.Commands) {
		t.Fatalf("catalogue length = %d, want %d", len(catalogue), len(spec.Commands))
	}
	groups := 0
	commands := 0
	for index, raw := range spec.Commands {
		switch raw.Kind {
		case "group":
			groups++
		case "command":
			commands++
		}
		want := CommandSpec{Path: raw.Path, Kind: raw.Kind, Options: convertParameters(raw.Options), Positionals: convertParameters(raw.Positionals)}
		if !reflect.DeepEqual(catalogue[index], want) {
			t.Fatalf("catalogue[%d] differs:\n got %#v\nwant %#v", index, catalogue[index], want)
		}
	}
	if groups != 37 || commands != 89 {
		t.Fatalf("catalogue kinds = %d groups, %d commands; want 37 and 89", groups, commands)
	}
}

func TestCLIV2GeneratedHelpCatalogue(t *testing.T) {
	files, err := filepath.Glob("../../specs/cli-v2/help/*.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 127 {
		t.Fatalf("help golden count = %d, want 127", len(files))
	}
	paths := []string{""}
	for _, command := range catalogue {
		paths = append(paths, command.Path)
	}
	if len(paths) != len(files) {
		t.Fatalf("catalogue help count = %d, want %d", len(paths), len(files))
	}
	for _, path := range paths {
		name := strings.ReplaceAll(path, " ", "-")
		if path == "" {
			name = "cq"
		}
		file := filepath.Join("../../specs/cli-v2/help", name+".txt")
		want, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := Help(path)
		if !ok {
			t.Errorf("Help(%q) missing", path)
			continue
		}
		if got != string(want) {
			t.Errorf("Help(%q) differs from %s", path, file)
		}
	}
	if _, ok := Help("codex proxy no-such-command"); ok {
		t.Fatal("unknown help path resolved")
	}
}

func TestCLIV2CompletionCatalogue(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		script, ok := Completion(shell)
		if !ok || !strings.HasSuffix(script, "\n") {
			t.Fatalf("Completion(%q) unavailable or lacks LF", shell)
		}
		for _, want := range []string{"codex", "proxy", "reserve", "token-refresh"} {
			if !strings.Contains(script, want) {
				t.Errorf("Completion(%q) lacks %q", shell, want)
			}
		}
		for _, flag := range []string{"help", "json", "version"} {
			if !strings.Contains(script, flag) {
				t.Errorf("Completion(%q) lacks %q flag", shell, flag)
			}
		}
		for _, hidden := range []string{"runtime-role", "linux-validation-child", "candidate-runtime", "namespace-helper"} {
			if strings.Contains(script, hidden) {
				t.Errorf("Completion(%q) exposes hidden command %q", shell, hidden)
			}
		}
	}
	if _, ok := Completion("powershell"); ok {
		t.Fatal("unsupported completion shell resolved")
	}
}

func TestCLIV2CompletionSyntax(t *testing.T) {
	for _, fixture := range []struct {
		shell string
		args  []string
	}{{"bash", []string{"-n"}}, {"zsh", []string{"-n"}}, {"fish", []string{"-n"}}} {
		path, err := exec.LookPath(fixture.shell)
		if err != nil {
			t.Errorf("%s unavailable; native completion gate required: %v", fixture.shell, err)
			continue
		}
		script, _ := Completion(fixture.shell)
		file := filepath.Join(t.TempDir(), "cq."+fixture.shell)
		if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
		command := exec.Command(path, append(fixture.args, file)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Errorf("%s completion syntax: %v\n%s", fixture.shell, err, output)
		}
		var sourceArgs []string
		switch fixture.shell {
		case "bash":
			sourceArgs = []string{"-c", `source "$1"`, "cq-completion-test", file}
		case "zsh":
			sourceArgs = []string{"-fc", `autoload -Uz compinit; compinit -D; source "$1"`, "cq-completion-test", file}
		case "fish":
			sourceArgs = []string{"-c", `source $argv[1]`, file}
		}
		command = exec.Command(path, sourceArgs...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Errorf("%s completion source: %v\n%s", fixture.shell, err, output)
		}
	}
}

func TestCLIV2CompletionBehaviour(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("bash unavailable; native completion gate required: %v", err)
	}
	script, _ := Completion("bash")
	file := filepath.Join(t.TempDir(), "cq.bash")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	probe := `source "$1"
COMP_WORDS=(cq check claude c); COMP_CWORD=3; _cq_complete
printf 'repeatable-enum:%s\n' "${COMPREPLY[*]}"
COMP_WORDS=(cq codex proxy pool set --account first --a); COMP_CWORD=7; _cq_complete
printf 'repeatable-option:%s\n' "${COMPREPLY[*]}"
COMP_WORDS=(cq codex proxy pool set -- 'Équipe bleue' --); COMP_CWORD=8; _cq_complete
printf 'after-stop:%s\n' "${COMPREPLY[*]}"
`
	command := exec.Command(bash, "-c", probe, "cq-completion-test", file)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("bash completion probe: %v\n%s", err, output)
	}
	got := string(output)
	repeatableEnum := strings.Split(strings.Split(got, "repeatable-enum:")[1], "\n")[0]
	if !strings.Contains(repeatableEnum, "codex") {
		t.Errorf("repeatable enum absent:\n%s", got)
	}
	if !strings.Contains(got, "repeatable-option:--account") {
		t.Errorf("repeatable option absent:\n%s", got)
	}
	if strings.Contains(strings.Split(got, "after-stop:")[1], "--") {
		t.Errorf("option suggested after --:\n%s", got)
	}
}
