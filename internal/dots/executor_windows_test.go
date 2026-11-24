//go:build windows

package dots

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestWindowsSecondaryExecutorPrivateStartup(t *testing.T) {
	anchor, err := CanonicalPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	privateWindowsDotsTestAnchor(t, anchor)
	system := Account{Alias: "11111111", AccountID: "system-id"}
	owner := Account{Alias: "22222222", AccountID: "secondary-id"}
	s := &Settings{
		RuntimeRoot: filepath.Join(anchor, "runtime"), AppPath: filepath.Join(anchor, "ChatGPT"),
		IntegrationMode: "native", SystemAlias: system.Alias, SecondaryAlias: owner.Alias,
		Dots: map[string]Dot{"secondary": {OwnerAlias: owner.Alias, OwnerAccountID: owner.AccountID, ProfileID: "secondary-profile"}},
	}
	if err := PrivateDirectory(s.RuntimeRoot); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"USERPROFILE", "HOME", "APPDATA", "LOCALAPPDATA", "TEMP", "TMP", "CODEX_HOME", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "HTTPS_PROXY"} {
		t.Setenv(key, "ambient-value")
	}
	t.Setenv("PATHEXT", ".EXE;.CMD")

	var installation string
	for range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		child := &executorTestChild{running: true}
		defaults := func(*Settings, *CatalogueClient, string) executorDependencies {
			return executorDependencies{
				inventory: func() (Inventory, error) { return Inventory{System: system, Accounts: []Account{system, owner}}, nil },
				request: func(_ context.Context, bound Account, host, method, path string, body []byte) (int, string, []byte, error) {
					var registration struct {
						Installation string `json:"installation_id"`
						Roots        []any  `json:"selected_capability_roots"`
					}
					if err := json.Unmarshal(body, &registration); err != nil || !nativeValidUUID(registration.Installation) || registration.Roots == nil || len(registration.Roots) != 0 || bound.AccountID != owner.AccountID || host != "chatgpt.com" || method != "POST" || path != "/backend-api/flora/cca/executor" {
						t.Fatalf("unexpected registration: %s %s %s: %v", method, host, path, err)
					}
					if installation != "" && installation != registration.Installation {
						t.Fatal("secondary installation identity changed")
					}
					installation = registration.Installation
					return 200, "application/json", []byte(`{"executor_id":"secondary-environment"}`), nil
				},
				launch: func(cfg *Settings, root string, port int, environment, capability string, sandbox, forward bool) (executorProcess, error) {
					if sandbox || forward || capability == "" || capability == "ambient-value" || environment != "secondary-environment" {
						t.Fatal("unexpected secondary launch options")
					}
					cmd, err := executorCommandFor(cfg, root, port, environment, capability, sandbox, forward)
					if err != nil {
						t.Fatal(err)
					}
					home, tmp, work := filepath.Join(root, "home"), filepath.Join(root, "tmp"), filepath.Join(root, "work")
					if cmd.Path != filepath.Join(s.AppPath, "app", "resources", "codex.exe") || cmd.Dir != work {
						t.Fatalf("unexpected secondary command %q in %q", cmd.Path, cmd.Dir)
					}
					for _, dir := range []string{root, home, tmp, work} {
						if err := privateDirectory(dir); err != nil {
							t.Fatalf("worker path %s: %v", dir, err)
						}
					}
					for key, want := range map[string]string{
						"USERPROFILE": home, "HOME": home, "APPDATA": home, "LOCALAPPDATA": home,
						"CODEX_HOME": home, "TEMP": tmp, "TMP": tmp, "TMPDIR": tmp,
						"CODEX_API_KEY": capability, "PATHEXT": ".EXE;.CMD",
					} {
						if got := environmentValue(cmd.Env, key); got != want {
							t.Errorf("unexpected executor %s", key)
						}
					}
					for _, key := range []string{"CODEX_ACCESS_TOKEN", "HTTPS_PROXY"} {
						if environmentValue(cmd.Env, key) != "" {
							t.Errorf("unexpected inherited %s", key)
						}
					}
					cancel()
					return child, nil
				},
			}
		}
		lookup := func(context.Context, *Settings, NativeEvaluator) (string, error) { return "primary-environment", nil }
		if err := runSecondaryExecutors(ctx, s, nil, nil, defaults, lookup); err != nil {
			t.Fatal(err)
		}
		if installation == "" || child.closed != 1 {
			t.Fatal("secondary executor did not start and stop")
		}
		state, err := executorRead(filepath.Join(s.RuntimeRoot, "executors", "state.json"), s.RuntimeRoot)
		if err != nil {
			t.Fatal(err)
		}
		owners, _ := state["owners"].(map[string]any)
		row, _ := owners[owner.Alias].(map[string]any)
		if row["environment_id"] != "secondary-environment" || row["active"] != false {
			t.Fatalf("unexpected stopped worker state: %v", row)
		}
	}
	work := filepath.Join(s.RuntimeRoot, "executors", owner.Alias, "work")
	windowsDotsTestACL(t, work, true)
	if _, err := executorDirectory(s, "executors", owner.Alias, "work"); err == nil {
		t.Fatal("accepted a writable worker directory")
	}
}

func TestWindowsExecutorResearchSandboxFailsClosed(t *testing.T) {
	s := &Settings{AppPath: t.TempDir()}
	if cmd, err := executorCommandFor(s, t.TempDir(), 12345, "isolated", "local", true, false); !errors.Is(err, errExecutorSandboxUnavailable) || cmd != nil {
		t.Fatal("Windows built a command for the macOS proof executor")
	}
	if err := RunLocalExecutor(context.Background(), s, nil, nil); !errors.Is(err, errExecutorSandboxUnavailable) {
		t.Fatalf("unexpected proof executor result: %v", err)
	}
	if _, _, err := ReuseNativeExecutor(context.Background(), s, nil, nil, nil); !errors.Is(err, errExecutorSandboxUnavailable) {
		t.Fatalf("unexpected proof executor reuse result: %v", err)
	}
}
