//go:build windows

package codex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"golang.org/x/sys/windows"
)

func TestWindowsAuthoritativeInventoryAcceptsCodexSandboxReader(t *testing.T) {
	computer, err := windows.ComputerName()
	if err != nil {
		t.Fatal(err)
	}
	sandbox, domain, kind, err := windows.LookupSID("", computer+`\CodexSandboxUsers`)
	if err != nil || !strings.EqualFold(domain, computer) || kind != windows.SidTypeAlias {
		t.Skipf("local Codex sandbox group unavailable: %v", err)
	}
	home := windowsInventoryHome(t, sandbox)
	fsys := fixedHomeDurableFS{OSFileSystem: fsutil.OSFileSystem{}, home: home}
	inventory, err := discoverAuthoritativeInventoryWithSources(context.Background(), fsys)
	if err != nil || len(inventory.Accounts) != 1 {
		t.Fatalf("Codex sandbox-readable system auth inventory = %d accounts, %v", len(inventory.Accounts), err)
	}
}

func TestWindowsAuthoritativeInventoryRejectsArbitraryReader(t *testing.T) {
	world, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	home := windowsInventoryHome(t, world)
	fsys := fixedHomeDurableFS{OSFileSystem: fsutil.OSFileSystem{}, home: home}
	if _, err := discoverAuthoritativeInventoryWithSources(context.Background(), fsys); !errors.Is(err, fsutil.ErrUnsafeSecurePath) {
		t.Fatalf("world-readable system auth error = %v, want unsafe path", err)
	}
}

func TestWindowsAuthoritativeInventoryAcceptsPrivateSystemAuth(t *testing.T) {
	home := windowsInventoryHome(t, nil)
	fsys := fixedHomeDurableFS{OSFileSystem: fsutil.OSFileSystem{}, home: home}
	inventory, err := discoverAuthoritativeInventoryWithSources(context.Background(), fsys)
	if err != nil || len(inventory.Accounts) != 1 {
		t.Fatalf("private system auth inventory = %d accounts, %v", len(inventory.Accounts), err)
	}
}

func windowsInventoryHome(t *testing.T, reader *windows.SID) string {
	t.Helper()
	home := t.TempDir()
	setWindowsManagedTestACL(t, home)
	codexDir := filepath.Join(home, ".codex")
	if err := os.Mkdir(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	setWindowsManagedTestACL(t, codexDir)
	path := filepath.Join(codexDir, "auth.json")
	data := codexAuthJSON("synthetic-access", "acct-1", fakeCodexJWT("user@example.test", "acct-1", "user-1", "plus"))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	sddl := fmt.Sprintf("O:%sD:P(A;;FA;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)", sid, sid)
	if reader != nil {
		readMask := uint32(windows.FILE_GENERIC_READ | windows.FILE_GENERIC_EXECUTE)
		sddl += fmt.Sprintf("(A;;0x%x;;;%s)", readMask, reader.String())
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
	return home
}
