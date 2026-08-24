//go:build windows

package keyring

import (
	"testing"

	"github.com/jacobcxdev/cq/internal/userdirs"
)

func TestCQManifestPathUsesWindowsCacheRoot(t *testing.T) {
	t.Setenv("USERPROFILE", `D:\attacker`)
	roots := userdirs.Roots{Cache: `C:\Users\alice\AppData\Local\cq\cache`}

	got := CQManifestPath(roots, `D:\attacker`)
	if got != `C:\Users\alice\AppData\Local\cq\cache\accounts.json` {
		t.Fatalf("CQManifestPath() = %q, want %q", got, `C:\Users\alice\AppData\Local\cq\cache\accounts.json`)
	}
}

func TestDefaultCQManifestPathUsesWindowsCacheRoot(t *testing.T) {
	originalRoots := resolveCQManifestRoots
	t.Cleanup(func() { resolveCQManifestRoots = originalRoots })
	resolveCQManifestRoots = func() (userdirs.Roots, error) {
		return userdirs.Roots{Cache: `C:\Users\alice\AppData\Local\cq\cache`}, nil
	}
	t.Setenv("USERPROFILE", `D:\attacker`)

	got, err := defaultCQManifestPath()
	if err != nil {
		t.Fatalf("defaultCQManifestPath() error = %v", err)
	}
	if got != `C:\Users\alice\AppData\Local\cq\cache\accounts.json` {
		t.Fatalf("defaultCQManifestPath() = %q, want %q", got, `C:\Users\alice\AppData\Local\cq\cache\accounts.json`)
	}
}
