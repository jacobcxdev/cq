//go:build windows

package app

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/provider"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"golang.org/x/sys/windows"
)

func TestWindowsCodexLoginPersistsAndLists(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString(fmt.Sprintf("D:P(A;OICI;FA;;;%s)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)", user.User.Sid))
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	flags := windows.SECURITY_INFORMATION(windows.OWNER_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	if err := windows.SetNamedSecurityInfo(home, windows.SE_FILE_OBJECT, flags, user.User.Sid, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(home, "state")
	fsys, err := fsutil.NewPrivateDestinationFileSystem(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	openControl := func() *codexprov.CredentialControl {
		t.Helper()
		store := &codexprov.ManagedStore{FS: fsys, Home: home, EnsureEpoch: func() error { return nil }}
		coordinator, err := codexprov.NewCredentialCoordinator(store, stateDir)
		if err != nil {
			t.Fatal(err)
		}
		control, err := codexprov.OpenCredentialControl(filepath.Join(stateDir, "credential.sock"), coordinator)
		if err != nil {
			t.Fatal(err)
		}
		return control
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	claims := auth.CodexClaims{Email: "person@example.test", AccountID: "acct-fixture", UserID: "user-fixture", PlanType: "plus"}
	tokens := auth.CodexTokenResponse{
		IDToken: appCodexJWT(claims.Email, claims.AccountID, claims.UserID), AccessToken: "fixture-access", RefreshToken: "fixture-refresh",
	}
	for range 2 {
		control := openControl()
		var output bytes.Buffer
		err := runCodexLoginWithAdmin(ctx, nil, false, fakeAppCodexLogin(tokens, claims), time.Now, &output, control)
		closeErr := control.Close()
		if err != nil || closeErr != nil || !strings.Contains(output.String(), "Logged in as "+claims.Email) {
			t.Fatalf("login error = %v, close error = %v, output = %q", err, closeErr, output.String())
		}
	}
	control := openControl()
	defer control.Close()
	accounts, err := accountListManager(provider.Codex, nil, control).Discover(ctx)
	if err != nil || len(accounts) != 1 || accounts[0].Email != claims.Email {
		t.Fatalf("account list count = %d, error = %v", len(accounts), err)
	}
}
