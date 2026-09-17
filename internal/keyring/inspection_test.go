package keyring

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
)

func TestInspectClaudeAccountsPreservesIdentityAndAuthority(t *testing.T) {
	rows, err := inspectClaudeAccounts(context.Background(), claudeInspectionReaders{
		home: "/isolated", manifest: "/isolated/manifest",
		readFile: func(path string) ([]byte, error) {
			if path == "/isolated/manifest" {
				return []byte(`[{"uuid":"a"},{"uuid":"b"}]`), nil
			}
			return []byte(`{"claudeAiOauth":{"accessToken":"private-a","accountUUID":"a","email":"same@example.com"}}`), nil
		},
		platform: func(context.Context) ([]claudeInspectionSource, error) {
			return []claudeInspectionSource{{account: ClaudeOAuth{AccessToken: "private-a"}, source: "platform_keychain", active: true}}, nil
		},
		get: func(_, id string) (string, error) {
			if id == "a" {
				return `{"accessToken":"private-a","accountUUID":"a","email":"same@example.com"}`, nil
			}
			return `{"accessToken":"private-b","accountUUID":"b","email":"same@example.com"}`, nil
		},
	})
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%d err=%v; wanted two distinct UUID identities", len(rows), err)
	}
	if !rows[0].Active || rows[1].Active || !reflect.DeepEqual(rows[0].Sources, []string{"cq_managed", "native_client", "platform_keychain"}) {
		t.Fatal("source or native-default authority lost")
	}
}

func TestInspectClaudeAccountsDoesNotHideSourceFailure(t *testing.T) {
	for _, source := range []string{"file", "manifest", "platform", "keyring"} {
		t.Run(source, func(t *testing.T) {
			_, err := inspectClaudeAccounts(context.Background(), claudeInspectionReaders{
				home: "/isolated", manifest: "/isolated/manifest",
				readFile: func(path string) ([]byte, error) {
					if (source == "manifest" && path == "/isolated/manifest") || (source == "file" && path != "/isolated/manifest") {
						return nil, os.ErrPermission
					}
					if path == "/isolated/manifest" {
						return []byte(`[{"uuid":"a"}]`), nil
					}
					return nil, os.ErrNotExist
				},
				platform: func(context.Context) ([]claudeInspectionSource, error) {
					if source == "platform" {
						return nil, os.ErrPermission
					}
					return nil, nil
				},
				get: func(string, string) (string, error) { return "", errors.New("private underlying failure") },
			})
			if err == nil {
				t.Fatal("incomplete inventory reported success")
			}
		})
	}
}

func TestInspectClaudeAccountsRetainsAmbiguousAnonymousIdentity(t *testing.T) {
	rows, err := inspectClaudeAccounts(context.Background(), claudeInspectionReaders{
		home: "/isolated", manifest: "/manifest",
		readFile: func(path string) ([]byte, error) {
			if path == "/manifest" {
				return nil, os.ErrNotExist
			}
			return []byte(`{"claudeAiOauth":{"accessToken":"shared"}}`), nil
		},
		platform: func(context.Context) ([]claudeInspectionSource, error) {
			return []claudeInspectionSource{
				{account: ClaudeOAuth{AccountUUID: "a", AccessToken: "shared", Email: "a@example.com"}, source: "platform_keychain"},
				{account: ClaudeOAuth{AccountUUID: "b", AccessToken: "shared", Email: "b@example.com"}, source: "platform_keychain"},
			}, nil
		},
		get: func(string, string) (string, error) { t.Fatal("unconfigured keyring read"); return "", nil },
	})
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows=%d err=%v; ambiguous native identity must remain separate", len(rows), err)
	}
	if rows[0].Account.AccountUUID != "" || !rows[0].Active || rows[1].Active || rows[2].Active {
		t.Fatal("ambiguous token affinity invented native default identity")
	}
}

func TestInspectClaudePlatformServicesChecksGapsAndErrors(t *testing.T) {
	calls := 0
	rows, err := inspectClaudePlatformServices(context.Background(), func(_ context.Context, service string) ([]byte, error) {
		calls++
		if service == "Claude Code-credentials-10" {
			return []byte(`{"claudeAiOauth":{"accessToken":"private","accountUUID":"a"}}`), nil
		}
		return nil, os.ErrNotExist
	})
	if err != nil || calls != 10 || len(rows) != 1 || rows[0].active {
		t.Fatalf("calls=%d rows=%d error=%v", calls, len(rows), err)
	}
	for _, data := range []string{"malformed", `{"claudeAiOauth":null}`, `{"claudeAiOauth":{}}`} {
		if _, err := inspectClaudePlatformServices(context.Background(), func(context.Context, string) ([]byte, error) { return []byte(data), nil }); err == nil {
			t.Fatal("malformed keychain entry suppressed")
		}
	}
	if _, err := inspectClaudePlatformServices(context.Background(), func(context.Context, string) ([]byte, error) { return nil, os.ErrPermission }); err == nil {
		t.Fatal("denied keychain access suppressed")
	}
}

func TestInspectClaudeAccountsEmptyAndCancellation(t *testing.T) {
	reads := 0
	readers := claudeInspectionReaders{home: "/isolated", manifest: "/manifest",
		readFile: func(string) ([]byte, error) { reads++; return nil, os.ErrNotExist },
		platform: func(context.Context) ([]claudeInspectionSource, error) { reads++; return nil, nil },
		get:      func(string, string) (string, error) { t.Fatal("unexpected undeclared keyring access"); return "", nil },
	}
	rows, err := inspectClaudeAccounts(context.Background(), readers)
	if err != nil || rows == nil || len(rows) != 0 || reads != 3 {
		t.Fatalf("rows=%v reads=%d err=%v", rows, reads, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reads = 0
	if _, err := inspectClaudeAccounts(ctx, readers); !errors.Is(err, context.Canceled) || reads != 0 {
		t.Fatalf("error=%v reads=%d", err, reads)
	}
}

func TestInspectClaudeAccountsManifestIdentity(t *testing.T) {
	for _, test := range []struct {
		name, manifest, raw string
		wantError           bool
	}{
		{"missing metadata", `[{"uuid":"known-id"}]`, `{"accessToken":"secret"}`, false},
		{"conflicting metadata", `[{"uuid":"known-id"}]`, `{"accessToken":"secret","accountUUID":"different-id"}`, true},
		{"null manifest", `null`, ``, true},
		{"empty identity", `[{}]`, `{"accessToken":"secret"}`, true},
		{"malformed credential", `[{"uuid":"known-id"}]`, `{"accessToken":`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			rows, err := inspectClaudeAccounts(context.Background(), claudeInspectionReaders{
				home: "/isolated", manifest: "/manifest",
				readFile: func(path string) ([]byte, error) {
					if path == "/manifest" {
						return []byte(test.manifest), nil
					}
					return nil, os.ErrNotExist
				},
				platform: func(context.Context) ([]claudeInspectionSource, error) { return nil, nil },
				get:      func(string, string) (string, error) { return test.raw, nil },
			})
			if (err != nil) != test.wantError {
				t.Fatalf("err=%v wanted error=%t", err, test.wantError)
			}
			if !test.wantError && (len(rows) != 1 || rows[0].Account.AccountUUID != "known-id") {
				t.Fatal("declared manifest identity lost")
			}
		})
	}
}

func TestInspectClaudeAccountsPropagatesCancellationDuringRead(t *testing.T) {
	for _, stage := range []string{"file", "platform", "manifest", "keyring"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, err := inspectClaudeAccounts(ctx, claudeInspectionReaders{
				home: "/isolated", manifest: "/manifest",
				readFile: func(path string) ([]byte, error) {
					if stage == "file" && path != "/manifest" || stage == "manifest" && path == "/manifest" {
						cancel()
						return nil, os.ErrPermission
					}
					if path == "/manifest" {
						return []byte(`[{"uuid":"id"}]`), nil
					}
					return nil, os.ErrNotExist
				},
				platform: func(context.Context) ([]claudeInspectionSource, error) {
					if stage == "platform" {
						cancel()
						return nil, os.ErrPermission
					}
					return nil, nil
				},
				get: func(string, string) (string, error) { cancel(); return "", os.ErrPermission },
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error=%v wanted cancellation", err)
			}
		})
	}
}
