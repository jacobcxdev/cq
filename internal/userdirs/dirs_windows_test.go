//go:build windows

package userdirs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveUsesWindowsAppData(t *testing.T) {
	got, err := (Resolver{
		RoamingAppData: func() (string, error) { return `C:\Users\alice\AppData\Roaming`, nil },
		LocalAppData:   func() (string, error) { return `C:\Users\alice\AppData\Local`, nil },
	}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	want := Roots{
		Config:  `C:\Users\alice\AppData\Roaming\cq`,
		State:   `C:\Users\alice\AppData\Local\cq\state`,
		Cache:   `C:\Users\alice\AppData\Local\cq\cache`,
		Runtime: `C:\Users\alice\AppData\Local\cq\runtime`,
		Logs:    `C:\Users\alice\AppData\Local\cq\logs`,
	}
	if got != want {
		t.Fatalf("roots = %#v, want %#v", got, want)
	}
	for _, root := range []string{got.Config, got.State, got.Cache, got.Runtime, got.Logs} {
		if !filepath.IsAbs(root) {
			t.Fatalf("root is not absolute: %q", root)
		}
	}
}

func TestResolveWindowsFailsWithoutAbsoluteAppData(t *testing.T) {
	tests := []struct {
		name   string
		config func() (string, error)
		cache  func() (string, error)
	}{
		{
			name:   "config error",
			config: func() (string, error) { return "", os.ErrPermission },
			cache:  func() (string, error) { return `C:\Local`, nil },
		},
		{
			name:   "relative config",
			config: func() (string, error) { return `relative`, nil },
			cache:  func() (string, error) { return `C:\Local`, nil },
		},
		{
			name:   "cache error",
			config: func() (string, error) { return `C:\Roaming`, nil },
			cache:  func() (string, error) { return "", os.ErrPermission },
		},
		{
			name:   "relative cache",
			config: func() (string, error) { return `C:\Roaming`, nil },
			cache:  func() (string, error) { return `relative`, nil },
		},
		{
			name:   "UNC config",
			config: func() (string, error) { return `\\server\share\Roaming`, nil },
			cache:  func() (string, error) { return `C:\Local`, nil },
		},
		{
			name:   "UNC cache",
			config: func() (string, error) { return `C:\Roaming`, nil },
			cache:  func() (string, error) { return `\\server\share\Local`, nil },
		},
		{
			name:   "drive-relative config",
			config: func() (string, error) { return `C:Roaming`, nil },
			cache:  func() (string, error) { return `C:\Local`, nil },
		},
		{
			name:   "extended config",
			config: func() (string, error) { return `\\?\C:\Roaming`, nil },
			cache:  func() (string, error) { return `C:\Local`, nil },
		},
		{
			name:   "device cache",
			config: func() (string, error) { return `C:\Roaming`, nil },
			cache:  func() (string, error) { return `\\.\C:\Local`, nil },
		},
		{
			name:   "alternate-data-stream config",
			config: func() (string, error) { return `C:\Roaming:stream`, nil },
			cache:  func() (string, error) { return `C:\Local`, nil },
		},
		{
			name:   "non-clean cache",
			config: func() (string, error) { return `C:\Roaming`, nil },
			cache:  func() (string, error) { return `C:\Local\..\Elsewhere`, nil },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := (Resolver{
				RoamingAppData: test.config,
				LocalAppData:   test.cache,
			}).Resolve()
			var diagnostic *EnvironmentError
			if !errors.As(err, &diagnostic) || diagnostic.Code != "environment_root_unavailable" || diagnostic.ExitCode != 4 || diagnostic.Error() != "Cannot resolve the user storage root." {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestResolveWindowsRejectsIncompleteResolver(t *testing.T) {
	_, err := (Resolver{}).Resolve()
	var diagnostic *EnvironmentError
	if !errors.As(err, &diagnostic) || diagnostic.Code != "environment_root_unavailable" || diagnostic.ExitCode != 4 || diagnostic.Error() != "Cannot resolve the user storage root." {
		t.Fatalf("error = %v", err)
	}
}

func TestDefaultIgnoresSpoofedApplicationDataEnvironment(t *testing.T) {
	attacker := filepath.Join(t.TempDir(), "attacker")
	attackerRoaming := filepath.Join(attacker, "roaming")
	attackerLocal := filepath.Join(attacker, "local")
	if err := os.MkdirAll(attackerRoaming, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(attackerLocal, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("APPDATA", attackerRoaming)
	t.Setenv("LOCALAPPDATA", attackerLocal)
	t.Setenv("USERPROFILE", attacker)

	anchors, err := WindowsAppDataAnchors()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if got.Config != filepath.Join(anchors.RoamingAppData, "cq") || got.State != filepath.Join(anchors.LocalAppData, "cq", "state") {
		t.Fatalf("roots = %#v, anchors = %#v", got, anchors)
	}
	for _, root := range []string{anchors.UserProfile, got.Config, got.State, got.Cache, got.Runtime, got.Logs} {
		if strings.HasPrefix(strings.ToLower(root), strings.ToLower(attacker)) {
			t.Fatalf("environment selected root %q", root)
		}
	}

	token, closeToken, err := openCurrentUserToken()
	if err != nil {
		t.Fatal(err)
	}
	expanded, expandErr := expandUserEnvironmentWith(
		token,
		`%USERPROFILE%\cq-native-token-probe`,
		callExpandEnvironmentStringsForUser,
	)
	closeErr := closeToken()
	if expandErr != nil || closeErr != nil {
		t.Fatalf("expand/close error = %v/%v", expandErr, closeErr)
	}
	if want := filepath.Join(anchors.UserProfile, "cq-native-token-probe"); expanded != want {
		t.Fatalf("token expansion = %q, want %q", expanded, want)
	}
}

func TestWindowsUserDirsSelectedAnchorIsLazy(t *testing.T) {
	for _, root := range []Root{ConfigRoot, StateRoot, CacheRoot, RuntimeRoot, LogsRoot} {
		calls := 0
		r := Resolver{Getenv: func(string) string { t.Fatal("read poisoned shell env"); return "" }, UserHomeDir: func() (string, error) { t.Fatal("read shell home"); return "", nil }, RoamingAppData: func() (string, error) {
			if root != ConfigRoot {
				t.Fatal("unused roaming anchor")
			}
			calls++
			return `C:\Subject\Roaming`, nil
		}, LocalAppData: func() (string, error) {
			if root == ConfigRoot {
				t.Fatal("unused local anchor")
			}
			calls++
			return `C:\Subject\Local`, nil
		}}
		if _, err := r.Resolve(root); err != nil || calls != 1 {
			t.Fatalf("error %v calls %d", err, calls)
		}
	}
}
