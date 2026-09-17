//go:build windows

package fsutil

import (
	"testing"

	"github.com/jacobcxdev/cq/internal/userdirs"
)

func TestWindowsCredentialHomeUsesAuthenticatedAnchor(t *testing.T) {
	anchors, err := userdirs.WindowsAppDataAnchors()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"USERPROFILE", "HOME", "APPDATA", "LOCALAPPDATA", "XDG_CONFIG_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(name, `C:\Poisoned`)
	}
	got, err := (OSFileSystem{}).UserHomeDir()
	if err != nil || got != anchors.UserProfile {
		t.Fatalf("home %q error %v; expected authenticated profile", got, err)
	}
	roots, err := userdirs.Default()
	if err != nil {
		t.Fatal(err)
	}
	if roots.Config == `C:\Poisoned\cq` {
		t.Fatal("poisoned root")
	}
}
