//go:build unix

package proxy

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestInitialiseCodexContinuityAuthorityOnDisk(t *testing.T) {
	t.Parallel()
	state := filepath.Join(t.TempDir(), "state")
	options := CodexContinuityOpenOptions{
		FS:          fsutil.OSFileSystem{},
		KeyPath:     filepath.Join(state, "leases.key"),
		JournalPath: filepath.Join(state, "leases.json"),
		Policy: CodexLeasePolicy{
			Retention: 24 * time.Hour,
			Now:       func() time.Time { return time.Date(2026, 8, 9, 3, 0, 0, 0, time.UTC) },
		},
		Modes: CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{}},
	}
	if err := InitialiseCodexContinuityAuthority(options, testCodexLeaseOwner{}); errors.Is(err, fsutil.ErrSecureCapabilityUnavailable) {
		t.Skip("kernel no-replace rename is unavailable")
	} else if err != nil {
		t.Fatal(err)
	}
	for path, wantMode := range map[string]os.FileMode{
		state:               0o700,
		options.KeyPath:     0o600,
		options.JournalPath: 0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != wantMode {
			t.Fatalf("mode for %s = %#o, want %#o", filepath.Base(path), got, wantMode)
		}
	}
	for _, path := range []string{freshCodexLeaseStagePath(options.KeyPath), freshCodexLeaseStagePath(options.JournalPath)} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stage %s remains: %v", filepath.Base(path), err)
		}
	}
	coordinator, err := OpenCodexContinuityCoordinator(options, testCodexLeaseOwner{})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Close(); err != nil {
		t.Fatal(err)
	}
}
