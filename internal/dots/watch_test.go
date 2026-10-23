package dots

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func accountWatchFixture(t *testing.T) *Settings {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	settings := &Settings{RuntimeRoot: filepath.Join(root, "runtime"), CQAccounts: filepath.Join(root, "accounts"), AccountSource: filepath.Join(root, "source"), SystemAuth: filepath.Join(root, "system.json")}
	for _, directory := range []string{settings.RuntimeRoot, settings.CQAccounts, settings.AccountSource} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{settings.SystemAuth, filepath.Join(root, "settings.json")} {
		if err := os.WriteFile(path, []byte("synthetic"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return settings
}

func TestAccountWatchFingerprintTracksMetadataOnly(t *testing.T) {
	settings := accountWatchFixture(t)
	fingerprint := func() string {
		value, err := accountWatchFingerprint(settings)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	previous := fingerprint()
	assertChanged := func() {
		t.Helper()
		current := fingerprint()
		if current == previous {
			t.Fatal("metadata change was missed")
		}
		previous = current
	}
	auth := filepath.Join(settings.CQAccounts, "account.json")
	if err := os.WriteFile(auth, []byte("unreadable credential"), 0o000); err != nil {
		t.Fatal(err)
	}
	assertChanged()
	if current := fingerprint(); current != previous {
		t.Fatal("unchanged metadata changed fingerprint")
	}
	if err := os.Chmod(auth, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(auth, []byte("synthetic refreshed credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertChanged()
	if err := os.Remove(auth); err != nil {
		t.Fatal(err)
	}
	assertChanged()
	for _, path := range []string{settings.SystemAuth, filepath.Join(settings.AccountSource, "managed-codex-accounts.json"), filepath.Join(filepath.Dir(settings.RuntimeRoot), "settings.json")} {
		if err := os.WriteFile(path, []byte("synthetic changed metadata"), 0o600); err != nil {
			t.Fatal(err)
		}
		assertChanged()
	}
	settings.AccountSource = ""
	if _, err := accountWatchFingerprint(settings); err != nil {
		t.Fatal(err)
	}
}

func TestAccountWatchChangedRevisionUsesLockedControllerRequest(t *testing.T) {
	settings := accountWatchFixture(t)
	calls := 0
	execute := func(ctx context.Context, request Request) (map[string]any, error) {
		calls++
		if request.Operation != "sync" || !request.Watcher {
			t.Fatalf("wrong controller request %#v", request)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 120*time.Second || time.Until(deadline) < 119*time.Second {
			t.Fatalf("missing bounded sync deadline %v", deadline)
		}
		return map[string]any{"discovery_complete": true, "discovery_retryable": false}, nil
	}
	revision, event := accountWatchOnce(context.Background(), settings, "", execute)
	if revision == "" || event != "account_sync_complete" {
		t.Fatalf("revision %q event %q", revision, event)
	}
	next, event := accountWatchOnce(context.Background(), settings, revision, execute)
	if next != revision || event != "" || calls != 1 {
		t.Fatalf("unchanged poll revision %q event %q calls %d", next, event, calls)
	}
}

func TestAccountWatchFailedRevisionRemainsPendingAndCanSettle(t *testing.T) {
	settings := accountWatchFixture(t)
	for _, test := range []struct {
		name    string
		outcome map[string]any
		err     error
		settled bool
	}{
		{"remote unavailable", map[string]any{"discovery_complete": false, "discovery_retryable": true}, nil, false},
		{"invalid local credential", map[string]any{"discovery_complete": false, "discovery_retryable": false}, nil, true},
		{"missing result", nil, nil, false},
		{"missing bool", map[string]any{"discovery_complete": true}, nil, false},
		{"wrong type", map[string]any{"discovery_complete": "yes", "discovery_retryable": false}, nil, false},
		{"operation busy", nil, errors.New("private error"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			execute := func(context.Context, Request) (map[string]any, error) {
				calls++
				if calls == 1 {
					return test.outcome, test.err
				}
				return map[string]any{"discovery_complete": true, "discovery_retryable": false}, nil
			}
			revision, event := accountWatchOnce(context.Background(), settings, "old", execute)
			if test.settled {
				if revision == "old" || event != "account_sync_complete" {
					t.Fatalf("revision %q event %q", revision, event)
				}
			} else if revision != "old" || event != "account_sync_failed" {
				t.Fatalf("failed revision %q event %q", revision, event)
			}
			next, event := accountWatchOnce(context.Background(), settings, revision, execute)
			if test.settled {
				if calls != 1 || next != revision || event != "" {
					t.Fatalf("settled local failure retried: calls %d revision %q event %q", calls, next, event)
				}
			} else if calls != 2 || next == "old" || event != "account_sync_complete" {
				t.Fatalf("retry failed: calls %d revision %q event %q", calls, next, event)
			}
		})
	}
}

func TestAccountWatchMetadataFailureStaysGenericAndNeverSyncs(t *testing.T) {
	settings := accountWatchFixture(t)
	settings.CQAccounts = "\x00private-path"
	revision, event := accountWatchOnce(context.Background(), settings, "old", func(context.Context, Request) (map[string]any, error) {
		t.Fatal("metadata failure attempted sync")
		return nil, nil
	})
	if revision != "old" || event != "account_sync_failed" || strings.Contains(event, "private") {
		t.Fatalf("revision %q event %q", revision, event)
	}
}

func TestAccountWatchBackoffIsBoundedAndResets(t *testing.T) {
	delay := accountWatchInterval
	for _, want := range []time.Duration{10, 20, 40, 60, 60, 60} {
		delay = accountWatchDelay(delay, "account_sync_failed")
		if delay != want*time.Second {
			t.Fatalf("delay %v want %vs", delay, want)
		}
	}
	for _, event := range []string{"account_sync_complete", ""} {
		if got := accountWatchDelay(time.Minute, event); got != 5*time.Second {
			t.Fatalf("reset delay %v", got)
		}
	}
}

func TestAccountWatchOwnsPrivateExclusiveLockAndStopsOnCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Dots watcher requires POSIX private locks")
	}
	settings := accountWatchFixture(t)
	lockPath := filepath.Join(settings.RuntimeRoot, "account-watch.lock")
	lock, err := fsutil.AcquireExclusiveLock(fsutil.OSFileSystem{}, lockPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RunAccountWatch(ctx, settings, Controller{}); !errors.Is(err, fsutil.ErrExclusiveLockHeld) {
		t.Fatalf("second watcher error %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := RunAccountWatch(ctx, settings, Controller{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled watcher error %v", err)
	}
	info, err := os.Lstat(lockPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("lock info %v error %v", info, err)
	}
}

func TestAccountWatchRejectsSymlinkLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink fixture")
	}
	settings := accountWatchFixture(t)
	outside := filepath.Join(filepath.Dir(settings.RuntimeRoot), "outside")
	if err := os.WriteFile(outside, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(settings.RuntimeRoot, "account-watch.lock")); err != nil {
		t.Fatal(err)
	}
	if err := RunAccountWatch(context.Background(), settings, Controller{}); err == nil {
		t.Fatal("symlink lock accepted")
	}
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != "unchanged" {
		t.Fatalf("symlink target %q error %v", data, err)
	}
}
