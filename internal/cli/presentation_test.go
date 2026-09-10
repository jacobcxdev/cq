package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCLIV2VersionPresentation(t *testing.T) {
	revision := strings.Repeat("a", 40)
	dirty := true
	got := VersionOutcome(BuildInfo{Version: "1.2.3", Revision: &revision, Dirty: &dirty})
	if got.ExitCode != 0 || got.Human != "cq 1.2.3\nRevision: "+revision+"\nCLI schema: 2\n" {
		t.Fatalf("VersionOutcome() = %#v", got)
	}
	var data struct {
		Version          string  `json:"version"`
		Revision         *string `json:"revision"`
		Dirty            *bool   `json:"dirty"`
		CLISchemaVersion int     `json:"cli_schema_version"`
	}
	if err := json.Unmarshal(got.Data, &data); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got.Data), "\x1b[") {
		t.Fatalf("version JSON contains ANSI: %q", got.Data)
	}
	if data.Version != "1.2.3" || data.Revision == nil || *data.Revision != revision || data.Dirty == nil || !*data.Dirty || data.CLISchemaVersion != 2 {
		t.Fatalf("version data = %#v", data)
	}
}

func TestCLIV2VersionUnknownProvenance(t *testing.T) {
	got := VersionOutcome(BuildInfo{})
	if got.Human != "cq dev\nRevision: unknown\nCLI schema: 2\n" {
		t.Fatalf("human version = %q", got.Human)
	}
	if string(got.Data) != `{"version":"dev","revision":null,"dirty":null,"cli_schema_version":2}` {
		t.Fatalf("version data = %s", got.Data)
	}
	if invalid := VersionOutcome(BuildInfo{Version: "release-ish"}); !strings.HasPrefix(invalid.Human, "cq dev\n") {
		t.Fatalf("invalid release version was reported: %q", invalid.Human)
	}
	if invalid := VersionOutcome(BuildInfo{Version: "1.2.3-01"}); !strings.HasPrefix(invalid.Human, "cq dev\n") {
		t.Fatalf("invalid semantic version was reported: %q", invalid.Human)
	}
	badRevision := "HEAD"
	if invalid := VersionOutcome(BuildInfo{Version: "1.2.3", Revision: &badRevision}); strings.Contains(invalid.Human, "HEAD") || strings.Contains(string(invalid.Data), `"HEAD"`) {
		t.Fatalf("invalid revision was reported: %#v", invalid)
	}
}

func TestCLIV2CompletionPresentation(t *testing.T) {
	got := CompletionOutcome("zsh")
	if got.ExitCode != 0 || got.Human == "" || strings.Contains(got.Human, "\x1b[") {
		t.Fatalf("completion outcome = %#v", got)
	}
	var data struct {
		Shell  string `json:"shell"`
		Script string `json:"script"`
	}
	if err := json.Unmarshal(got.Data, &data); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got.Data), "\x1b[") {
		t.Fatalf("completion JSON contains ANSI: %q", got.Data)
	}
	if data.Shell != "zsh" || data.Script != got.Human {
		t.Fatalf("completion data = %#v", data)
	}
}
