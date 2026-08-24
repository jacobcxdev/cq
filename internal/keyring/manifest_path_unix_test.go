//go:build unix

package keyring

import (
	"testing"

	"github.com/jacobcxdev/cq/internal/userdirs"
)

func TestCQManifestPathPreservesUnixCompatibilityPath(t *testing.T) {
	roots := userdirs.Roots{Cache: "/Users/alice/Library/Caches/cq"}

	got := CQManifestPath(roots, "/Users/alice")
	if got != "/Users/alice/.cache/cq/accounts.json" {
		t.Fatalf("CQManifestPath() = %q, want %q", got, "/Users/alice/.cache/cq/accounts.json")
	}
}

func TestDefaultCQManifestPathPreservesUnixCompatibilityPath(t *testing.T) {
	originalRoots := resolveCQManifestRoots
	originalHome := resolveCQManifestHome
	t.Cleanup(func() {
		resolveCQManifestRoots = originalRoots
		resolveCQManifestHome = originalHome
	})
	resolveCQManifestRoots = func() (userdirs.Roots, error) {
		return userdirs.Roots{Cache: "/Users/alice/Library/Caches/cq"}, nil
	}
	resolveCQManifestHome = func() (string, error) { return "/Users/alice", nil }

	got, err := defaultCQManifestPath()
	if err != nil {
		t.Fatalf("defaultCQManifestPath() error = %v", err)
	}
	if got != "/Users/alice/.cache/cq/accounts.json" {
		t.Fatalf("defaultCQManifestPath() = %q, want %q", got, "/Users/alice/.cache/cq/accounts.json")
	}
}
