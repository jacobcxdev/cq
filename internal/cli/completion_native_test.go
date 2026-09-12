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
	for _, shell := range []string{"bash", "zsh"} {
		t.Run(shell, func(t *testing.T) {
			binary := "/bin/" + shell
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
