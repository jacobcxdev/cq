//go:build windows

package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	zaiprov "github.com/jacobcxdev/cq/internal/provider/zai"
	"github.com/jacobcxdev/cq/internal/provider/zai/zcode"
	"golang.org/x/sys/windows"
)

type isolatedZCodeCommandFS struct {
	fsutil.OSFileSystem
	home string
}

func (fsys isolatedZCodeCommandFS) UserHomeDir() (string, error) { return fsys.home, nil }

func grokCommandFixtureFS(t *testing.T) (fsutil.FileSystem, string) {
	t.Helper()
	root := t.TempDir()
	setWindowsCQFixtureACL(t, root, false)
	dir := filepath.Join(root, "cq")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	setWindowsCQFixtureACL(t, dir, true)
	fsys, err := fsutil.NewPrivateDestinationFileSystem(dir)
	if err != nil {
		t.Fatal(err)
	}
	return fsys, dir
}

func TestWindowsZCodeDeviceInOwnerControlledCQRoot(t *testing.T) {
	root := t.TempDir()
	setWindowsCQFixtureACL(t, root, false)
	dir := filepath.Join(root, "cq")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	setWindowsCQFixtureACL(t, dir, true)
	scoped, err := fsutil.NewPrivateDestinationFileSystem(dir)
	if err != nil {
		t.Fatal(err)
	}
	fsys := isolatedZCodeCommandFS{OSFileSystem: scoped, home: root}
	installed := filepath.Join(root, "absent", "telemetry-state.json")
	first, err := ensureZCodeCommandDevice(fsys, dir, installed)
	if err != nil || !zcodeResetUUID(first) {
		t.Fatalf("first device registration: %v", err)
	}
	second, err := ensureZCodeCommandDevice(fsys, dir, installed)
	if err != nil || second != first {
		t.Fatalf("repeat device registration: %v", err)
	}
	if err := fsutil.ValidateOwnerControlledDirectory(fsys, dir); err != nil {
		t.Fatalf("CQ root authority changed: %v", err)
	}
	if err := fsutil.ValidateSecureRegularFile(fsys, filepath.Join(dir, "zcode-device.json")); err != nil {
		t.Fatalf("private device file: %v", err)
	}
	var output, diagnostics bytes.Buffer
	deps := zaiTestDependencies(fsys, &output)
	deps.configDir = func() (string, error) { return dir, nil }
	deps.deviceMid = func() (string, error) { return ensureZCodeCommandDevice(fsys, dir, installed) }
	deps.errOutput = &diagnostics
	deps.openURL = func(context.Context, string) error { return nil }
	deps.client = zaiCommandDoer(func(*http.Request) (*http.Response, error) {
		t.Fatal("fixture login sent HTTP")
		return nil, nil
	})
	deps.oauthLogin = func(ctx context.Context, client zcode.OAuthClient) (zcode.LoginSession, error) {
		if client.DeviceMid != first {
			t.Fatal("callback changed device identity")
		}
		if err := client.OpenURL(ctx, "https://chat.z.ai/fixture-login"); err != nil {
			return zcode.LoginSession{}, err
		}
		return zcode.LoginSession{UserID: "windows-user", Email: "windows@example.test", ZCodeJWT: "zcode-fixture", RawOAuth: "oauth-fixture", BusinessJWT: "business-fixture"}, nil
	}
	for range 2 {
		output.Reset()
		if err := runZAICommand(t.Context(), "login", ZAICmd{Login: ZAILoginCmd{ZCode: true}}, true, deps); err != nil {
			t.Fatalf("save fake browser callback: %v", err)
		}
		output.Reset()
		if err := runZAICommand(t.Context(), "accounts", ZAICmd{}, true, deps); err != nil {
			t.Fatalf("list repeat callback account: %v", err)
		}
	}
	accounts, err := zaiprov.ReadAccounts(fsys, func(string) string { return "" }, dir)
	if err != nil || len(accounts) != 1 || accounts[0].OAuth == nil || accounts[0].OAuth.DeviceMid != first {
		t.Fatalf("persisted callback accounts = %d, %v", len(accounts), err)
	}
	for _, name := range []string{"zcode-accounts.key", "zcode-accounts.json", "zai.json"} {
		if err := fsutil.ValidateSecureRegularFile(fsys, filepath.Join(dir, name)); err != nil {
			t.Fatalf("private %s: %v", name, err)
		}
	}
}

func setWindowsCQFixtureACL(t *testing.T, path string, readable bool) {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	sddl := fmt.Sprintf("O:%sD:P(A;OICI;FA;;;%s)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)", sid, sid)
	if readable {
		sddl += "(A;OICI;FR;;;WD)"
	}
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, owner, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}
