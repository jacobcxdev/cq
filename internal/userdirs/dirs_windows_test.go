//go:build windows

package userdirs

import (
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
		want   string
	}{
		{
			name:   "config error",
			config: func() (string, error) { return "", os.ErrPermission },
			cache:  func() (string, error) { return `C:\Local`, nil },
			want:   "resolve Windows roaming data",
		},
		{
			name:   "relative config",
			config: func() (string, error) { return `relative`, nil },
			cache:  func() (string, error) { return `C:\Local`, nil },
			want:   "Windows roaming data is not a clean absolute local drive path",
		},
		{
			name:   "cache error",
			config: func() (string, error) { return `C:\Roaming`, nil },
			cache:  func() (string, error) { return "", os.ErrPermission },
			want:   "resolve Windows local data",
		},
		{
			name:   "relative cache",
			config: func() (string, error) { return `C:\Roaming`, nil },
			cache:  func() (string, error) { return `relative`, nil },
			want:   "Windows local data is not a clean absolute local drive path",
		},
		{
			name:   "UNC config",
			config: func() (string, error) { return `\\server\share\Roaming`, nil },
			cache:  func() (string, error) { return `C:\Local`, nil },
			want:   "Windows roaming data is not a clean absolute local drive path",
		},
		{
			name:   "UNC cache",
			config: func() (string, error) { return `C:\Roaming`, nil },
			cache:  func() (string, error) { return `\\server\share\Local`, nil },
			want:   "Windows local data is not a clean absolute local drive path",
		},
		{
			name:   "drive-relative config",
			config: func() (string, error) { return `C:Roaming`, nil },
			cache:  func() (string, error) { return `C:\Local`, nil },
			want:   "Windows roaming data is not a clean absolute local drive path",
		},
		{
			name:   "extended config",
			config: func() (string, error) { return `\\?\C:\Roaming`, nil },
			cache:  func() (string, error) { return `C:\Local`, nil },
			want:   "Windows roaming data is not a clean absolute local drive path",
		},
		{
			name:   "device cache",
			config: func() (string, error) { return `C:\Roaming`, nil },
			cache:  func() (string, error) { return `\\.\C:\Local`, nil },
			want:   "Windows local data is not a clean absolute local drive path",
		},
		{
			name:   "alternate-data-stream config",
			config: func() (string, error) { return `C:\Roaming:stream`, nil },
			cache:  func() (string, error) { return `C:\Local`, nil },
			want:   "Windows roaming data is not a clean absolute local drive path",
		},
		{
			name:   "non-clean cache",
			config: func() (string, error) { return `C:\Roaming`, nil },
			cache:  func() (string, error) { return `C:\Local\..\Elsewhere`, nil },
			want:   "Windows local data is not a clean absolute local drive path",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := (Resolver{
				RoamingAppData: test.config,
				LocalAppData:   test.cache,
			}).Resolve()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestResolveWindowsRejectsIncompleteResolver(t *testing.T) {
	_, err := (Resolver{}).Resolve()
	if err == nil || !strings.Contains(err.Error(), "incomplete resolver") {
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
