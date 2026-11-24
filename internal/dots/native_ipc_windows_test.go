//go:build windows

package dots

import (
	"context"
	"path/filepath"
	"testing"
)

func TestWindowsNativeBrokerPrivatePipeFirstAndRepeat(t *testing.T) {
	anchor := t.TempDir()
	privateWindowsDotsTestAnchor(t, anchor)
	root := filepath.Join(anchor, "dots", "runtime")
	if err := PrivateDirectory(root); err != nil {
		t.Fatal(err)
	}
	path, _, err := dotsPipePath(root, "native-debug")
	if err != nil {
		t.Fatal(err)
	}
	if path == filepath.Join(root, nativeSocketName) {
		t.Fatal("Windows native endpoint used a filesystem socket")
	}
	for i := 0; i < 2; i++ {
		broker, err := newNativeBroker(root)
		if err != nil {
			t.Fatalf("open broker %d: %v", i, err)
		}
		broker.appPID = 12345
		if _, err := newNativeBroker(root); err == nil {
			broker.close()
			t.Fatal("second broker replaced live named pipe")
		}
		done := make(chan error, 1)
		go func() {
			defer func() {
				if recover() != nil {
					done <- context.Canceled
				}
			}()
			done <- broker.once(context.Background())
		}()
		identity, err := NativeIdentity(context.Background(), &Settings{RuntimeRoot: root})
		if err != nil || identity["app_pid"] != 12345 {
			broker.close()
			t.Fatalf("native identity %d = %v, %v", i, identity, err)
		}
		if err := <-done; err != nil {
			broker.close()
			t.Fatal(err)
		}
		broker.close()
		if _, err := NativeIdentity(context.Background(), &Settings{RuntimeRoot: root}); err == nil {
			t.Fatal("closed named pipe remained available")
		}
	}
}

func TestWindowsNativePipeRootAndPurposeIsolation(t *testing.T) {
	anchor := t.TempDir()
	privateWindowsDotsTestAnchor(t, anchor)
	first := filepath.Join(anchor, "first")
	second := filepath.Join(anchor, "second")
	for _, root := range []string{first, second} {
		if err := PrivateDirectory(root); err != nil {
			t.Fatal(err)
		}
	}
	a, _, err := dotsPipePath(first, "native-debug")
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := dotsPipePath(second, "native-debug")
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := dotsPipePath(first, "cloud-gateway")
	if err != nil {
		t.Fatal(err)
	}
	if a == b || a == c || b == c {
		t.Fatal("Dots private pipe names collided across roots or purposes")
	}
	if _, _, err := dotsPipePath("relative", "native-debug"); err == nil {
		t.Fatal("relative runtime root accepted")
	}
}
