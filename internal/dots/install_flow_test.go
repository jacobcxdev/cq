package dots

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// This exercises the real installation/state/launch/awareness path with only
// the qualified application pin, upstream HTTP and native renderer substituted.
// It deliberately never starts desktop processes or background workers.
func TestFreshGoInstallationDiscoveredPrimedAndUpgradedWithoutHistoryPollution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native POSIX launchers")
	}
	ctx := context.Background()
	options, pin := installationFixture(t)
	options.Root = filepath.Join(filepath.Dir(options.Root), "fresh CQ ' installation")
	if err := os.WriteFile(options.Executable, []byte("#!/bin/sh\nprintf '%s\\000' \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	catalogueTestWrite(t, options.SystemAuth, catalogueTestDocument("fixture-owner", now.Unix()+3600))
	if err := PrivateDirectory(options.CQAccounts); err != nil {
		t.Fatal(err)
	}
	secondaryPath := filepath.Join(options.CQAccounts, "secondary.auth.json")
	catalogueTestWrite(t, secondaryPath, catalogueTestDocument("fixture-secondary", now.Unix()+3600))
	profile := filepath.Join(filepath.Dir(options.Root), "Library", "Application Support", "com.openai.chat")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	profileMarker := filepath.Join(profile, "existing-profile-marker")
	if err := os.WriteFile(profileMarker, []byte("preserve the ordinary desktop profile"), 0o600); err != nil {
		t.Fatal(err)
	}
	protected := map[string][]byte{}
	for _, path := range []string{options.SystemAuth, secondaryPath, profileMarker, filepath.Join(options.App, "Contents", "Resources", "app.asar")} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		protected[path] = data
	}
	result, err := installPrepared(ctx, options, pin)
	if err != nil || result["installed"] != true || result["runtime"] != "go" || result["system_app_modified"] != false {
		t.Fatalf("fresh installation %#v, %v", result, err)
	}
	root, err := CanonicalPath(options.Root)
	if err != nil {
		t.Fatal(err)
	}
	c := Controller{Root: root, Executable: options.Executable, BaseURL: "http://127.0.0.1:12345"}
	s, err := c.settings()
	if err != nil || len(s.Dots) != 0 {
		t.Fatalf("fresh state %#v, %v", s, err)
	}
	secondaryName, secondaryRoot := "Same", "root-fixture-secondary"
	c.Catalogue = &CatalogueClient{Settings: s, Now: func() time.Time { return now }, Origin: "https://synthetic.invalid", HTTP: catalogueTestDoer(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Host != "synthetic.invalid" || request.URL.Path != "/backend-api/tbo/primary" {
			t.Fatalf("unexpected discovery request %s %s", request.Method, request.URL)
		}
		owner := request.Header.Get("ChatGPT-Account-Id")
		switch owner {
		case "fixture-owner":
			return catalogueTestResponse(http.StatusOK, catalogueTestPrimary(owner, "Same", "root-"+owner)), nil
		case "fixture-secondary":
			return catalogueTestResponse(http.StatusOK, catalogueTestPrimary(owner, secondaryName, secondaryRoot)), nil
		default:
			t.Fatalf("foreign discovery owner %q", owner)
			return nil, nil
		}
	})}
	changed, accounts, err := c.Refresh(ctx)
	if err != nil || !changed || len(accounts) != 2 {
		t.Fatalf("discovery changed %v, accounts %#v, %v", changed, accounts, err)
	}
	s, err = c.settings()
	if err != nil || len(s.Dots) != 2 {
		t.Fatalf("discovered state %#v, %v", s, err)
	}
	handles := dotHandles(s)
	for _, handle := range handles {
		dot := s.Dots[handle]
		if dot.DisplayName != "Same" || handle != StableHandle(dot.OwnerAccountID, dot.ProfileID) {
			t.Fatal("duplicate display names changed routing identity")
		}
	}
	staged, err := StageExecutable(options.Executable, s.RuntimeRoot)
	if err != nil || staged != helperExecutable(s) {
		t.Fatalf("staged helper %q, %v", staged, err)
	}
	for name, value := range map[string]string{"port": "32123", "realtime-port": "32124"} {
		if err := os.WriteFile(filepath.Join(s.RuntimeRoot, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env, _, _ := launchFixtureEnvironment(t, s)
	args, launchEnv, err := prepareLaunch(s, c.BaseURL, c.Executable, env)
	if err != nil || environmentValue(launchEnv, "CODEX_HOME") != filepath.Dir(options.SystemAuth) || environmentValue(launchEnv, "OPENAI_BASE_URL") != c.BaseURL {
		t.Fatalf("normal-profile launch %#v, %v", args, err)
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "--user-data-dir") || strings.HasPrefix(arg, "--remote-debugging-port") {
			t.Fatalf("fresh native launch created a separate profile: %q", arg)
		}
	}
	launcher := filepath.Join(filepath.Dir(options.Root), "Applications", "ChatGPT via CQ.app", "Contents", "MacOS", "launch")
	output, err := exec.CommandContext(ctx, launcher).Output()
	if err != nil || !bytes.Equal(output, []byte(strings.Join([]string{"__dots", "desktop-launch", root}, "\x00")+"\x00")) {
		t.Fatalf("installed launcher argv %q, %v", output, err)
	}
	sends := map[string]int{}
	c.Native = nativeEvaluationFunc(func(_ context.Context, expression string) ([]any, error) {
		if strings.Contains(expression, "safePost(") {
			_, payload, ok := strings.Cut(expression, "const expected=")
			if !ok {
				t.Fatal("native setup omitted explicit target")
			}
			payload, _, ok = strings.Cut(payload, ";\nconst primary=")
			var expected struct{ Profile, Text, Request string }
			if !ok || json.Unmarshal([]byte(payload), &expected) != nil {
				t.Fatal("native setup target could not be decoded")
			}
			handle := ""
			for h, dot := range s.Dots {
				if dot.ProfileID == expected.Profile {
					handle = h
				}
			}
			if handle == "" || !strings.Contains(expected.Text, "Your stable routing handle is "+handle) || !strings.Contains(expected.Text, "[CQ:directory:") || !strings.Contains(expected.Text, "Display names can be duplicated") || expected.Request == "" {
				t.Fatal("native setup lost target identity or discovery instructions")
			}
			sends[handle]++
			return []any{map[string]any{"accepted": true, "message_id": "synthetic-" + expected.Request}}, nil
		}
		rows := []any{}
		for _, h := range dotHandles(s) {
			dot := s.Dots[h]
			rows = append(rows, map[string]any{"id": dot.ProfileID, "name": dot.DisplayName})
		}
		return []any{rows}, nil
	})
	awareness := NewAwareness(s, c.client(s), c.Native)
	if rows, err := awareness.Reconcile(ctx); err != nil || len(rows) != 2 {
		t.Fatalf("initial setup %#v, %v", rows, err)
	}
	for _, h := range handles {
		if sends[h] != 1 {
			t.Fatalf("initial setup delivered %d times to %s", sends[h], h)
		}
	}
	if _, err := c.Execute(ctx, Request{Operation: "prime", Target: "Same"}); err == nil {
		t.Fatal("ambiguous display name silently selected an owner")
	}
	if rows, err := awareness.Reconcile(ctx); err != nil || len(rows) != 2 || rows[0]["skipped"] != true || rows[1]["skipped"] != true {
		t.Fatalf("repeated startup redelivered setup: %#v, %v", rows, err)
	}
	ledger := map[string][]byte{}
	if err := filepath.WalkDir(filepath.Join(s.RuntimeRoot, "awareness"), func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		ledger[path] = data
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := installPrepared(ctx, options, pin); err != nil {
		t.Fatal("upgrade failed", err)
	}
	secondaryName, secondaryRoot = "Null", "new-root-fixture-secondary"
	if changed, _, err := c.Refresh(ctx); err != nil || !changed {
		t.Fatalf("post-upgrade rename/root refresh failed: %v %v", changed, err)
	}
	s, err = c.settings()
	if err != nil || !slices.Equal(handles, dotHandles(s)) {
		t.Fatalf("upgrade changed stable handles: %#v, %v", s, err)
	}
	awareness = NewAwareness(s, c.client(s), c.Native)
	if rows, err := awareness.Reconcile(ctx); err != nil || len(rows) != 2 || rows[0]["skipped"] != true || rows[1]["skipped"] != true {
		t.Fatalf("upgrade redelivered setup: %#v, %v", rows, err)
	}
	for path, before := range ledger {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("upgrade changed setup ledger %q: %v", path, err)
		}
	}
	target := StableHandle("fixture-secondary", "profile-fixture-secondary")
	if result, err := c.Execute(ctx, Request{Operation: "prime", Target: target}); err != nil || len(result["priming"].([]map[string]any)) != 1 {
		t.Fatalf("manual re-prime %#v, %v", result, err)
	}
	if _, err := awareness.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	for _, h := range handles {
		want := 1
		if h == target {
			want = 2
		}
		if sends[h] != want {
			t.Fatalf("setup sends for %s: got %d, want %d", h, sends[h], want)
		}
	}
	for path, before := range protected {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("installation changed protected app/profile/credentials %q: %v", path, err)
		}
	}
	for _, name := range []string{"venv", "user-data", "codex-home", "cloud_stdio.py", "probe.py"} {
		if _, err := os.Stat(filepath.Join(s.RuntimeRoot, name)); !os.IsNotExist(err) {
			t.Fatalf("fresh native Go installation created legacy/separate-profile state %q: %v", name, err)
		}
	}
	if err := AppCheck(s); err != nil {
		t.Fatal("signed application contents changed", err)
	}
}
