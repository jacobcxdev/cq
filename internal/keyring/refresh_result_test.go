package keyring

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRefreshWriteResultRetainsFileCommit(t *testing.T) {
	for _, cancelAfter := range []bool{false, true} {
		t.Run(map[bool]string{false: "later store failure", true: "cancellation"}[cancelAfter], func(t *testing.T) {
			home := t.TempDir()
			setKeyringTestHome(t, home)
			acct := ClaudeOAuth{Email: "test@example.com", AccountUUID: "id", AccessToken: "old"}
			data, _ := json.Marshal(ClaudeCredentials{ClaudeAiOauth: &acct})
			path := filepath.Join(home, ".claude", ".credentials.json")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			acct.AccessToken = "new"
			result := persistRefreshedTokenResult(ctx, &acct, refreshPersistenceOperations{
				write: func(c *ClaudeCredentials) error {
					b, _ := json.Marshal(c)
					err := os.WriteFile(path, b, 0600)
					if cancelAfter {
						cancel()
					}
					return err
				},
				update: func(string, *ClaudeCredentials) error { return errors.New("keychain failed") },
				store: func(*ClaudeOAuth) CredentialWriteResult {
					return CredentialWriteResult{Err: errors.New("store failed")}
				},
			})
			if !result.Changed || result.Err == nil {
				t.Fatal("lost committed file change or later failure")
			}
			if cancelAfter && !errors.Is(result.Err, context.Canceled) {
				t.Fatal("lost cancellation")
			}
		})
	}
}
func TestRefreshWriteResultRetainsKeyringCommit(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		t.Run(map[bool]string{false: "manifest failure", true: "delete then retry failure"}[deleting], func(t *testing.T) {
			home := t.TempDir()
			setKeyringTestHome(t, home)
			t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "config"))
			// A regular file in place of the config directory prevents manifest publication.
			if !deleting {
				path, err := defaultCQManifestPath()
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(filepath.Dir(path)), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Dir(path), []byte("blocked"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			sets, deleted := 0, false
			var diagnostics []string
			ctx := WithDiagnostics(context.Background(), func(code, message string) {
				diagnostics = append(diagnostics, code+": "+message)
			})
			result := storeCQAccountResultContext(ctx, &ClaudeOAuth{AccountUUID: "id"}, func(string, string, string) error {
				sets++
				if deleting {
					return errors.New("set failed")
				}
				return nil
			}, func(string, string) error { deleted = true; return nil })
			if !result.Changed || result.Err == nil {
				t.Fatal("lost committed keyring mutation or failure")
			}
			if !deleting && (len(diagnostics) != 1 || diagnostics[0] != "manifest_directory_failed: Claude credential manifest directory could not be created.") {
				t.Fatalf("unexpected manifest diagnostics: %v", diagnostics)
			}
			if deleting && len(diagnostics) != 0 {
				t.Fatalf("unexpected retry diagnostics: %v", diagnostics)
			}
			if !deleting && (deleted || sets != 1) {
				t.Fatal("manifest failure did not follow a successful initial Set")
			}
			if deleting && (!deleted || sets != 2) {
				t.Fatal("fallback did not execute")
			}
		})
	}
}
func TestRefreshWriteResultOnlyProjectsMatchingNativeIdentity(t *testing.T) {
	for _, matching := range []bool{false, true} {
		t.Run(map[bool]string{false: "different identity", true: "matching identity"}[matching], func(t *testing.T) {
			home := t.TempDir()
			setKeyringTestHome(t, home)
			native := ClaudeOAuth{AccountUUID: "native", Email: "native@test.invalid", AccessToken: "native-old"}
			if err := WriteCredentialsFile(&ClaudeCredentials{ClaudeAiOauth: &native}); err != nil {
				t.Fatal(err)
			}
			refreshed := ClaudeOAuth{AccountUUID: "other", Email: "other@test.invalid", AccessToken: "refreshed"}
			if matching {
				refreshed.AccountUUID = native.AccountUUID
				refreshed.Email = native.Email
			}
			writes, updates, stores := 0, 0, 0
			result := persistRefreshedTokenResult(context.Background(), &refreshed, refreshPersistenceOperations{
				write:  func(c *ClaudeCredentials) error { writes++; return WriteCredentialsFile(c) },
				update: func(string, *ClaudeCredentials) error { updates++; return nil },
				store: func(c *ClaudeOAuth) CredentialWriteResult {
					stores++
					if c.AccountUUID != refreshed.AccountUUID {
						t.Fatal("stored wrong identity")
					}
					return CredentialWriteResult{Changed: true}
				},
			})
			if result.Err != nil || !result.Changed || stores != 1 {
				t.Fatal("managed persistence failed")
			}
			if matching && (writes != 1 || updates != 1) || !matching && (writes != 0 || updates != 0) {
				t.Fatal("projected a different native identity")
			}
			data, err := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
			if err != nil {
				t.Fatal(err)
			}
			var actual ClaudeCredentials
			if err := json.Unmarshal(data, &actual); err != nil {
				t.Fatal(err)
			}
			want := "native-old"
			if matching {
				want = "refreshed"
			}
			if actual.ClaudeAiOauth.AccessToken != want {
				t.Fatal("native material changed unexpectedly")
			}
		})
	}
}
