//go:build windows

package dots

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/userdirs"
	"golang.org/x/sys/windows"
)

func TestWindowsPrivateDotsStateCreateAndRepeat(t *testing.T) {
	anchor := t.TempDir()
	privateWindowsDotsTestAnchor(t, anchor)
	root := filepath.Join(anchor, "dots")
	if err := PrivateDirectory(root); err != nil {
		t.Fatalf("create private root: %v", err)
	}
	path := filepath.Join(root, "state.json")
	for _, value := range []string{"first", "repeat"} {
		if err := SaveJSON(path, map[string]string{"value": value}); err != nil {
			t.Fatalf("save private state: %v", err)
		}
		if data, err := ReadOwned(path, root); err != nil || len(data) == 0 {
			t.Fatalf("read private state: %v", err)
		}
	}
	fsys, err := fsutil.NewPrivateDestinationFileSystem(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.ValidateSecureDirectory(fsys, root); err != nil {
		t.Fatalf("private root ACL: %v", err)
	}
	if err := fsutil.ValidateSecureRegularFile(fsys, path); err != nil {
		t.Fatalf("private state ACL: %v", err)
	}
}

func TestWindowsDotsInstallPreservesPrivateStateWithoutLaunching(t *testing.T) {
	anchor := t.TempDir()
	privateWindowsDotsTestAnchor(t, anchor)
	credentials := filepath.Join(anchor, "credentials")
	if err := PrivateDirectory(credentials); err != nil {
		t.Fatal(err)
	}
	claims, _ := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "fixture-owner"}})
	document, _ := json.Marshal(map[string]any{"tokens": map[string]string{"account_id": "fixture-owner", "id_token": "header." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"}})
	authPath := filepath.Join(credentials, "auth.json")
	fsys, err := windowsStateFS(credentials)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.SecureAtomicWrite(fsys, authPath, document); err != nil {
		t.Fatal(err)
	}
	options := InstallOptions{
		Root: filepath.Join(anchor, "dots"), App: windowsAppFixture(t),
		SystemAuth: authPath, CQAccounts: filepath.Join(anchor, "accounts"),
		Mode: "native", Executable: filepath.Join(anchor, "cq.exe"),
		launcher: func(string, string, string) (string, error) {
			return filepath.Join(anchor, windowsDotsShortcutName), nil
		},
	}
	if err := os.WriteFile(options.Executable, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	var original []byte
	for index := range 2 {
		result, err := Install(context.Background(), options)
		if err != nil || result["installed"] != true || result["system_app_modified"] != false {
			t.Fatalf("install %d = %#v, %v", index, result, err)
		}
		settings, err := LoadSettings(filepath.Join(options.Root, "settings.json"))
		if err != nil || settings.AppPath != options.App || settings.IntegrationMode != "native" {
			t.Fatalf("installed settings %d: %#v, %v", index, settings, err)
		}
		key, err := ReadOwned(filepath.Join(options.Root, "runtime", "leaf-key.pem"), filepath.Join(options.Root, "runtime"))
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			original = key
		} else if !bytes.Equal(original, key) {
			t.Fatal("repeat install replaced private certificate key")
		}
	}
	updated, _, err := QualifyWindowsApp(windowsAppFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	controller := Controller{
		Root:          options.Root,
		currentAppPin: func() (Settings, error) { return updated, nil },
		appRunning:    func(context.Context, *Settings) (bool, error) { return true, nil },
	}
	settings, err := controller.settings()
	if err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(options.Root, "settings.json")
	before, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := OperationLock(t.Context(), options.Root, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := controller.refreshAppPin(settings, nil); err == nil || !strings.Contains(err.Error(), "active Desktop") {
		t.Fatalf("active Desktop update accepted: %v", err)
	}
	if _, err := controller.Status(t.Context()); err == nil || !strings.Contains(err.Error(), "updated") {
		t.Fatalf("read-only stale status accepted: %v", err)
	}
	after, err := os.ReadFile(settingsPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("active/status probe changed settings: %v", err)
	}
	controller.appRunning = func(context.Context, *Settings) (bool, error) { return false, nil }
	if err := controller.refreshAppPin(settings, nil); err != nil {
		t.Fatalf("idle Windows package re-pin: %v", err)
	}
	loaded, err := controller.settings()
	if err != nil || loaded.AppPath != updated.AppPath || loaded.SystemAccountID != "fixture-owner" {
		t.Fatalf("re-pinned settings: %#v, %v", loaded, err)
	}
}

func TestWindowsDotsShortcutFirstRepeatAndForeignPreserved(t *testing.T) {
	anchor := t.TempDir()
	privateWindowsDotsTestAnchor(t, anchor)
	programs := filepath.Join(anchor, "Programs")
	if err := PrivateDirectory(programs); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(programs, windowsDotsShortcutName)
	executable := filepath.Join(anchor, "cq.exe")
	if err := os.WriteFile(executable, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(anchor, "package")
	if err := os.MkdirAll(filepath.Join(app, "app"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "app", "ChatGPT.exe"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(anchor, "dots")
	for range 2 {
		got, err := installWindowsDotsLauncherAt(path, root, app, executable)
		if err != nil || got != path {
			t.Fatalf("install Start Menu shortcut: %q, %v", got, err)
		}
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := installWindowsDotsLauncherAt(path, filepath.Join(anchor, "other"), app, executable); err == nil {
		t.Fatal("foreign installation replaced existing shortcut")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("foreign install changed existing shortcut: %v", err)
	}
}

func TestWindowsDotsStagedHelperAndOperationLock(t *testing.T) {
	anchor := t.TempDir()
	privateWindowsDotsTestAnchor(t, anchor)
	root := filepath.Join(anchor, "dots")
	runtimeRoot := filepath.Join(root, "runtime")
	if err := PrivateDirectory(runtimeRoot); err != nil {
		t.Fatal(err)
	}
	lock, err := OperationLock(t.Context(), root, 0)
	if err != nil {
		t.Fatalf("first operation lock: %v", err)
	}
	if second, err := OperationLock(t.Context(), root, 0); err == nil {
		second.Close()
		t.Fatal("concurrent Dots operation lock accepted")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	lock, err = OperationLock(t.Context(), root, 0)
	if err != nil {
		t.Fatalf("repeat operation lock: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(anchor, "cq.exe")
	for _, value := range []string{"first", "repeat"} {
		if err := os.WriteFile(source, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		staged, err := StageExecutable(source, runtimeRoot)
		if err != nil || staged != filepath.Join(runtimeRoot, "cq-dots-runtime.exe") {
			t.Fatalf("stage Windows helper: %q, %v", staged, err)
		}
		data, err := os.ReadFile(staged)
		if err != nil || string(data) != value {
			t.Fatalf("staged helper content: %q, %v", data, err)
		}
		if err := windowsValidatePrivateFile(staged); err != nil {
			t.Fatalf("staged helper ACL: %v", err)
		}
	}
}

func TestWindowsDotsCurrentUserStartMenuReadOnly(t *testing.T) {
	if os.Getenv("CQ_TEST_WINDOWS_DOTS_START_MENU") != "1" {
		t.Skip("set CQ_TEST_WINDOWS_DOTS_START_MENU=1 for read-only Start Menu ACL qualification")
	}
	anchors, err := userdirs.WindowsAppDataAnchors()
	if err != nil {
		t.Fatal(err)
	}
	programs := filepath.Join(anchors.RoamingAppData, "Microsoft", "Windows", "Start Menu", "Programs")
	if err := fsutil.ValidateOwnerControlledDirectory(fsutil.OSFileSystem{}, programs); err != nil {
		t.Fatalf("current-user Start Menu directory is not owner controlled: %v", err)
	}
}

func TestWindowsDotsPrivateStateRejectsWritableAndReparseRoots(t *testing.T) {
	for _, kind := range []string{"writable", "reparse"} {
		t.Run(kind, func(t *testing.T) {
			anchor := t.TempDir()
			privateWindowsDotsTestAnchor(t, anchor)
			root := filepath.Join(anchor, "dots")
			if kind == "writable" {
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
				windowsDotsTestACL(t, root, true)
			} else {
				actual := filepath.Join(anchor, "actual")
				if err := PrivateDirectory(actual); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(actual, root); err != nil {
					t.Skipf("directory reparse fixture unavailable: %v", err)
				}
			}
			if err := PrivateDirectory(root); err == nil {
				t.Fatal("unsafe Dots private root accepted")
			}
		})
	}
}

func privateWindowsDotsTestAnchor(t *testing.T, path string) {
	t.Helper()
	windowsDotsTestACL(t, path, false)
}

func windowsDotsTestACL(t *testing.T, path string, writable bool) {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	sddl := fmt.Sprintf("O:%sD:P(A;OICI;FA;;;%s)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)", sid, sid)
	if writable {
		sddl += "(A;OICI;FA;;;WD)"
	}
	security, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := security.Owner()
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := security.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, owner, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(path); err != nil || !info.IsDir() {
		t.Fatalf("fixture anchor: %v", err)
	}
}
