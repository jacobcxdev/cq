package cache

import (
	"testing"

	"github.com/jacobcxdev/cq/internal/userdirs"
)

func TestDirUsesResolvedCacheRoot(t *testing.T) {
	roots := userdirs.Roots{Cache: "/cq/cache"}
	if got := Dir(roots); got != "/cq/cache" {
		t.Fatalf("Dir() = %q, want /cq/cache", got)
	}
}
