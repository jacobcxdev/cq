//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package codex

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestDefaultRecoveringRefreshControlForwardsRecoveryRecorder(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	home := shortEndpointDir(t)
	stateDir := filepath.Dir(DefaultCredentialControlPath(home))
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := DefaultCredentialControlPath(home)
	makeExactCredentialEndpointOrphan(t, path, "orphan-generation")

	var calls atomic.Int32
	control, err := OpenDefaultRecoveringCredentialRefreshControlWithLegacyMaintenanceVerifierAndRecoveryRecorder(
		context.Background(), fixedHomeDurableFS{home: home}, http.DefaultClient, nil,
		CredentialEndpointRecoveryRecorderFunc(func() error {
			calls.Add(1)
			return nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if !control.Owner() {
		t.Fatal("recorded refresh control recovery did not produce an owner")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("recovery recorder calls = %d, want 1", got)
	}
}
