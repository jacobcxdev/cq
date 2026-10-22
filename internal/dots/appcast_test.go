package dots

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeAppcastPreservesInstallationAndUnknownFields(t *testing.T) {
	for _, mode := range []os.FileMode{0o600, 0o644} {
		t.Run(mode.String(), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "production-appcast-bootstrap.json")
			original := []byte(`{"backendAppcastEnabled":true,"installationId":"existing-installation","unknown":{"large":9007199254740993}}`)
			if err := os.WriteFile(path, original, mode); err != nil {
				t.Fatal(err)
			}
			if err := prepareNativeAppcast(path); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var state map[string]json.RawMessage
			if err := json.Unmarshal(data, &state); err != nil {
				t.Fatal(err)
			}
			if string(state["backendAppcastEnabled"]) != "false" || string(state["installationId"]) != `"existing-installation"` || string(state["unknown"]) != `{"large":9007199254740993}` {
				t.Fatal("updater state did not preserve installation and unknown fields")
			}
			info, _ := os.Stat(path)
			if info.Mode().Perm() != mode {
				t.Fatal("native state permissions changed")
			}
			if err := prepareNativeAppcast(path); err != nil {
				t.Fatal(err)
			}
			after, _ := os.Stat(path)
			if !os.SameFile(info, after) || info.ModTime() != after.ModTime() {
				t.Fatal("repeated launch rewrote already corrected state")
			}
		})
	}
}

func TestNativeAppcastLeavesFreshAndAlreadyDisabledProfilesAlone(t *testing.T) {
	for _, original := range []string{"", `{"backendAppcastEnabled":false,"installationId":"existing"}`, `{"installationId":"existing"}`} {
		t.Run(original, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "production-appcast-bootstrap.json")
			if original != "" {
				if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := prepareNativeAppcast(path); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if original == "" {
				if !os.IsNotExist(err) {
					t.Fatal("fresh profile acquired synthetic updater state")
				}
			} else if err != nil || string(data) != original {
				t.Fatal("unrelated updater state changed")
			}
		})
	}
	if err := prepareNativeAppcast(filepath.Join(t.TempDir(), "absent-profile", "state.json")); err != nil {
		t.Fatal(err)
	}
}

func TestNativeAppcastRejectsInvalidStateWithoutChanges(t *testing.T) {
	for _, original := range []string{`{`, `[]`, `null`, `{"backendAppcastEnabled":"true"}`, `{"backendAppcastEnabled":null}`, strings.Repeat(" ", 1<<20+1)} {
		path := filepath.Join(t.TempDir(), "state.json")
		if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := prepareNativeAppcast(path); err == nil {
			t.Fatal("invalid updater state accepted")
		}
		data, _ := os.ReadFile(path)
		if !bytes.Equal(data, []byte(original)) {
			t.Fatal("invalid state overwritten")
		}
	}
}

func TestNativeAppcastRejectsSymlinksAndSharedWrites(t *testing.T) {
	for _, variant := range []string{"file-link", "directory-link", "shared-file", "shared-directory"} {
		t.Run(variant, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "state.json")
			original := []byte(`{"backendAppcastEnabled":true}`)
			if err := os.WriteFile(path, original, 0o644); err != nil {
				t.Fatal(err)
			}
			target := path
			switch variant {
			case "file-link":
				target = filepath.Join(directory, "target.json")
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "directory-link":
				alias := filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(directory, alias); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(alias, "state.json")
			case "shared-file":
				if err := os.Chmod(path, 0o666); err != nil {
					t.Fatal(err)
				}
			case "shared-directory":
				if err := os.Chmod(directory, 0o777); err != nil {
					t.Fatal(err)
				}
			}
			if err := prepareNativeAppcast(path); err == nil {
				t.Fatal("unsafe updater state accepted")
			}
			data, _ := os.ReadFile(target)
			if !bytes.Equal(data, original) {
				t.Fatal("unsafe state overwritten")
			}
		})
	}
}
