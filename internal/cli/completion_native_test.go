package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCLIV2CompletionNativeInsertion(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("python3 unavailable; native completion gate required: %v", err)
	}
	shells := []struct{ name, shell, binary string }{{"bash", "bash", "/bin/bash"}, {"zsh", "zsh", "/bin/zsh"}}
	if bash, err := exec.LookPath("bash"); err == nil {
		stock, stockErr := os.Stat("/bin/bash")
		selected, selectedErr := os.Stat(bash)
		if stockErr == nil && selectedErr == nil && !os.SameFile(stock, selected) {
			shells = append(shells, struct{ name, shell, binary string }{"bash-path", "bash", bash})
		}
	}
	for _, fixture := range shells {
		shell, binary := fixture.shell, fixture.binary
		t.Run(fixture.name, func(t *testing.T) {
			if _, err := os.Stat(binary); err != nil {
				t.Fatalf("%s unavailable; native completion gate required: %v", binary, err)
			}
			script, _ := Completion(shell)
			file := filepath.Join(t.TempDir(), "cq."+shell)
			if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, fixture := range []struct {
				name, input string
				want        []string
			}{
				{"inline-enum", "codex proxy fixture create --content-encoding=zs", []string{"codex", "proxy", "fixture", "create", "--content-encoding=zstd"}},
				{"prior-inline-option", "codex proxy fixture create --content-encoding=gzip --input=Éq", []string{"codex", "proxy", "fixture", "create", "--content-encoding=gzip", "--input=Équipe bleue.json"}},
				{"literal-equals-after-stop", "check -- =", []string{"check", "--", "="}},
				{"option-like-after-stop", "check -- --content-encoding=zs", []string{"check", "--", "--content-encoding=zs"}},
				{"literal-equals-value", "codex proxy fixture create --input =", []string{"codex", "proxy", "fixture", "create", "--input", "="}},
				{"inline-path", "codex proxy fixture create --input=Éq", []string{"codex", "proxy", "fixture", "create", "--input=Équipe bleue.json"}},
				{"separate-path", "codex proxy fixture create --input Éq", []string{"codex", "proxy", "fixture", "create", "--input", "Équipe bleue.json"}},
				{"command-directory-collision", "ser", []string{"service"}},
			} {
				t.Run(fixture.name, func(t *testing.T) {
					command := exec.Command(python, "testdata/native_completion.py", binary, file, "cq "+fixture.input)
					output, err := command.CombinedOutput()
					if err != nil {
						t.Fatalf("native Tab/Enter: %v\n%s", err, output)
					}
					var got []string
					if err := json.Unmarshal(output, &got); err != nil {
						t.Fatalf("native argv: %v\n%s", err, output)
					}
					t.Logf("native Tab/Enter argv: %q", got)
					if !reflect.DeepEqual(got, fixture.want) {
						t.Fatalf("native Tab/Enter argv = %q, want %q", got, fixture.want)
					}
				})
			}
		})
	}
}
