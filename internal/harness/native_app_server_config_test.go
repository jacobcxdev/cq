package harness

import (
	"encoding/json"
	"testing"
)

func TestNativeAppManagedConfigWarningRequiresOnlyKnownField(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		message string
		wantErr bool
	}{
		{"known", "Codex is ignoring 1 unrecognized configuration setting.\n  user (config.toml): `computer_use.windows.always_allowed_app_ids` is ignored.", false},
		{"other", "Codex is ignoring 1 unrecognized configuration setting.\n  user (config.toml): `bogus` is ignored.", true},
		{"both", "Codex is ignoring 2 unrecognized configuration settings.\n  user (config.toml): `bogus` is ignored.\n  user (config.toml): `computer_use.windows.always_allowed_app_ids` is ignored.", true},
		{"missing", "", true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			output := &nativeConfigOutput{}
			line, err := json.Marshal(map[string]any{"fields": map[string]string{"message": scenario.message}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := output.Write(append(line, '\n')); err != nil {
				t.Fatal(err)
			}
			if gotErr := output.validateAppManagedWarning(); (gotErr != nil) != scenario.wantErr {
				t.Fatalf("warning validation error = %v, want error %v", gotErr, scenario.wantErr)
			}
		})
	}
}
