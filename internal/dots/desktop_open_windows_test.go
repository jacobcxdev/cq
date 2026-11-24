//go:build windows

package dots

import (
	"context"
	"path/filepath"
	"testing"
)

func TestWindowsDesktopActivationRejectsInvalidOrCancelledPackage(t *testing.T) {
	if err := openDesktopApp(context.Background(), filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing package was eligible for Desktop activation")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := openDesktopApp(ctx, windowsAppFixture(t)); err == nil {
		t.Fatal("cancelled Desktop activation created a process")
	}
}
