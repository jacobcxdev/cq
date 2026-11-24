//go:build windows

package dots

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestWindowsNativeSendKeepsPrivateReceiptAndPreventsRetry(t *testing.T) {
	anchor := t.TempDir()
	privateWindowsDotsTestAnchor(t, anchor)
	root := filepath.Join(anchor, "runtime")
	if err := PrivateDirectory(root); err != nil {
		t.Fatal(err)
	}
	root = filepath.Join(root, "coordination", "attempt")
	if err := coordinationDirectory(root); err != nil {
		t.Fatalf("create private message receipt directory: %v", err)
	}
	ledger := filepath.Join(root, "transport.json")
	s := &Settings{SystemAlias: "system", Dots: map[string]Dot{
		"primary": {OwnerAlias: "system", ProfileID: "first"},
		"peer":    {OwnerAlias: "secondary", ProfileID: "second"},
	}}
	fsys, err := dotsFileSystem(ledger)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	e := nativeEvaluationFunc(func(context.Context, string) ([]any, error) {
		calls++
		if err := fsutil.ValidateSecureRegularFile(fsys, ledger); err != nil {
			t.Fatalf("pending receipt must be private before sending: %v", err)
		}
		return []any{map[string]any{"accepted": true}}, nil
	})
	if result, err := NativeSend(context.Background(), s, e, "peer", "fixture", ledger); err != nil || result["outcome"] != "accepted" {
		t.Fatalf("save native send receipt: %v %v", result, err)
	}
	if _, err := NativeSend(context.Background(), s, e, "peer", "fixture", ledger); !errors.Is(err, os.ErrExist) || calls != 1 {
		t.Fatalf("existing receipt did not prevent another send: %v (calls %d)", err, calls)
	}
}

func TestWindowsDotsWorkersUsePrivateCustomRoot(t *testing.T) {
	anchor := t.TempDir()
	privateWindowsDotsTestAnchor(t, anchor)
	root := filepath.Join(anchor, "dots", "runtime")
	if err := PrivateDirectory(root); err != nil {
		t.Fatal(err)
	}
	s := &Settings{RuntimeRoot: root}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RunAccountWatch(ctx, s, Controller{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("account watcher did not hold the private runtime lock: %v", err)
	}
	if err := RunRoomCoordination(ctx, s, nil, nil); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("room coordination did not hold the private runtime lock: %v", err)
	}
}
