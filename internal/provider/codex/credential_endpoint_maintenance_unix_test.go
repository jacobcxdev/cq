//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestInspectLegacyCredentialEndpointCapturesExactRefusedSocket(t *testing.T) {
	t.Parallel()
	path := createRefusedLegacyCredentialSocket(t)

	snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Version != 1 || snapshot.Path != path || snapshot.State != LegacyCredentialEndpointRefused {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if snapshot.Directory.Type != "directory" || snapshot.Directory.Mode != 0o700 || snapshot.Directory.UID != uint64(os.Geteuid()) {
		t.Fatalf("directory proof = %#v", snapshot.Directory)
	}
	if snapshot.Socket.Type != "socket" || snapshot.Socket.Mode != 0o600 || snapshot.Socket.UID != uint64(os.Geteuid()) || snapshot.Socket.Links != 1 {
		t.Fatalf("socket proof = %#v", snapshot.Socket)
	}

	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseLegacyCredentialEndpointSnapshot(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed, snapshot) {
		t.Fatalf("parsed snapshot = %#v, want %#v", parsed, snapshot)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("inspect mutated legacy socket: %v", err)
	}
}

func TestInspectLegacyCredentialEndpointRejectsLiveOwner(t *testing.T) {
	t.Parallel()
	dir := shortEndpointDir(t)
	path := filepath.Join(dir, "credential.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	defer func() {
		_ = listener.Close()
		_ = os.Remove(path)
	}()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := InspectLegacyCredentialEndpoint(context.Background(), path); !errors.Is(err, ErrLegacyCredentialEndpointNotRefused) {
		t.Fatalf("inspect live error = %v, want not-refused", err)
	}
}

func TestParseLegacyCredentialEndpointSnapshotRejectsDuplicateAndUnknownFields(t *testing.T) {
	t.Parallel()
	path := createRefusedLegacyCredentialSocket(t)
	snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := append([]byte(`{"version":1,`), data[1:]...)
	if _, err := ParseLegacyCredentialEndpointSnapshot(duplicate); err == nil {
		t.Fatal("duplicate version error = nil")
	}
	unknown := append(data[:len(data)-1], []byte(`,"unknown":true}`)...)
	if _, err := ParseLegacyCredentialEndpointSnapshot(unknown); err == nil {
		t.Fatal("unknown field error = nil")
	}
}

func TestParseLegacyCredentialEndpointTransitionTicketRejectsNonCanonicalProofs(t *testing.T) {
	t.Parallel()
	path := createRefusedLegacyCredentialSocket(t)
	snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	transition, err := PrepareLegacyCredentialEndpointTransition(context.Background(), path, snapshot, DrainAuthorityFunc(func(context.Context, string) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	defer transition.Close()
	data, err := json.Marshal(transition.Ticket())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseLegacyCredentialEndpointTransitionTicket(data); err != nil {
		t.Fatalf("parse valid ticket: %v", err)
	}
	invalid := [][]byte{
		append([]byte(`{"version":1,`), data[1:]...),
		append(data[:len(data)-1], []byte(`,"unknown":true}`)...),
		append(append([]byte(nil), data...), []byte(` {}`)...),
	}
	wrongOwner := transition.Ticket()
	wrongOwner.Lock.UID++
	wrongOwnerData, err := json.Marshal(wrongOwner)
	if err != nil {
		t.Fatal(err)
	}
	invalid = append(invalid, wrongOwnerData)
	for index, candidate := range invalid {
		if _, err := ParseLegacyCredentialEndpointTransitionTicket(candidate); err == nil {
			t.Fatalf("invalid ticket %d parsed successfully", index)
		}
	}
}

func TestPrepareLegacyCredentialEndpointTransitionQuarantinesExactSocket(t *testing.T) {
	t.Parallel()
	path := createRefusedLegacyCredentialSocket(t)
	snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	assertions := 0
	authority := DrainAuthorityFunc(func(_ context.Context, gotPath string) error {
		assertions++
		if gotPath != path {
			t.Fatalf("drain path = %q, want %q", gotPath, path)
		}
		return nil
	})

	transition, err := PrepareLegacyCredentialEndpointTransition(context.Background(), path, snapshot, authority)
	if err != nil {
		t.Fatal(err)
	}
	defer transition.Close()
	if assertions < 5 {
		t.Fatalf("drain assertions = %d, want checks before lock-bound mutations", assertions)
	}
	ticket := transition.Ticket()
	if ticket.Path != path || ticket.ID == "" || ticket.Directory != snapshot.Directory || ticket.Socket != snapshot.Socket || ticket.QuarantineName == "" {
		t.Fatalf("ticket = %#v", ticket)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canonical socket error = %v, want absent", err)
	}
	quarantinePath := filepath.Join(filepath.Dir(path), ticket.QuarantineName)
	quarantineInfo, err := os.Lstat(quarantinePath)
	if err != nil || quarantineInfo.Mode()&os.ModeSocket == 0 {
		t.Fatalf("quarantine = %#v, %v; want socket", quarantineInfo, err)
	}
	journalInfo, err := os.Lstat(credentialEndpointMaintenanceJournalPath(path))
	if err != nil || !journalInfo.Mode().IsRegular() || journalInfo.Mode().Perm() != 0o600 {
		t.Fatalf("journal = %#v, %v; want private regular file", journalInfo, err)
	}
	lockInfo, err := os.Lstat(credentialEndpointLockPath(path))
	if err != nil || !lockInfo.Mode().IsRegular() || lockInfo.Mode().Perm() != 0o600 {
		t.Fatalf("lock = %#v, %v; want private regular file", lockInfo, err)
	}
	contender, err := fsutil.AcquireExclusiveLock(fsutil.OSFileSystem{}, credentialEndpointLockPath(path))
	if contender != nil {
		_ = contender.Close()
	}
	if !errors.Is(err, fsutil.ErrExclusiveLockHeld) {
		t.Fatalf("contender lock error = %v, want held", err)
	}
}

func TestOpenCredentialEndpointRejectsAnyMaintenanceJournal(t *testing.T) {
	t.Parallel()
	for _, allowRecovery := range []bool{false, true} {
		allowRecovery := allowRecovery
		t.Run(fmt.Sprintf("recovery=%t", allowRecovery), func(t *testing.T) {
			t.Parallel()
			dir := shortEndpointDir(t)
			path := filepath.Join(dir, "credential.sock")
			journalPath := credentialEndpointMaintenanceJournalPath(path)
			if err := os.WriteFile(journalPath, []byte("not-json"), 0o600); err != nil {
				t.Fatal(err)
			}

			endpoint, client, err := openCredentialEndpoint(path, allowRecovery, nil)
			if endpoint != nil {
				_ = endpoint.Close()
			}
			if client != nil {
				_ = client.Close()
			}
			if !errors.Is(err, ErrCredentialEndpointMaintenancePending) {
				t.Fatalf("open error = %v, want maintenance pending", err)
			}
			for _, unexpected := range []string{path, credentialEndpointSidecarPath(path), credentialEndpointLockPath(path)} {
				if _, statErr := os.Lstat(unexpected); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("open created %s: %v", unexpected, statErr)
				}
			}
		})
	}
}

func TestOpenCredentialEndpointRejectsUnsafeMaintenanceJournalEntry(t *testing.T) {
	t.Parallel()
	dir := shortEndpointDir(t)
	path := filepath.Join(dir, "credential.sock")
	if err := os.Symlink("missing-journal-target", credentialEndpointMaintenanceJournalPath(path)); err != nil {
		t.Fatal(err)
	}
	endpoint, client, err := openCredentialEndpoint(path, true, nil)
	if endpoint != nil {
		_ = endpoint.Close()
	}
	if client != nil {
		_ = client.Close()
	}
	if !errors.Is(err, ErrCredentialEndpointMaintenancePending) {
		t.Fatalf("open error = %v, want maintenance pending", err)
	}
	for _, unexpected := range []string{path, credentialEndpointSidecarPath(path), credentialEndpointLockPath(path)} {
		if _, statErr := os.Lstat(unexpected); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("open created %s: %v", unexpected, statErr)
		}
	}
}

func TestLegacyCredentialEndpointTransitionCommitRemovesTransactionButRetainsLock(t *testing.T) {
	t.Parallel()
	path := createRefusedLegacyCredentialSocket(t)
	snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	authority := DrainAuthorityFunc(func(context.Context, string) error { return nil })
	transition, err := PrepareLegacyCredentialEndpointTransition(context.Background(), path, snapshot, authority)
	if err != nil {
		t.Fatal(err)
	}
	ticket := transition.Ticket()
	if err := transition.Commit(context.Background()); err != nil {
		_ = transition.Close()
		t.Fatal(err)
	}
	if transition.State() != CredentialEndpointMaintenanceCommitted {
		t.Fatalf("state = %q, want committed", transition.State())
	}
	for _, absent := range []string{path, filepath.Join(filepath.Dir(path), ticket.QuarantineName), credentialEndpointMaintenanceJournalPath(path)} {
		if _, statErr := os.Lstat(absent); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("committed path %s error = %v, want absent", absent, statErr)
		}
	}
	if _, err := os.Lstat(credentialEndpointLockPath(path)); err != nil {
		t.Fatalf("permanent lock error = %v", err)
	}
	if err := transition.Close(); err != nil {
		t.Fatal(err)
	}
	lock, err := fsutil.AcquireExclusiveLock(fsutil.OSFileSystem{}, credentialEndpointLockPath(path))
	if err != nil {
		t.Fatalf("acquire retained lock: %v", err)
	}
	_ = lock.Close()
}

func TestLegacyCredentialEndpointTransitionRollbackRestoresExactSocketAndParksJournal(t *testing.T) {
	t.Parallel()
	path := createRefusedLegacyCredentialSocket(t)
	snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	authority := DrainAuthorityFunc(func(context.Context, string) error { return nil })
	transition, err := PrepareLegacyCredentialEndpointTransition(context.Background(), path, snapshot, authority)
	if err != nil {
		t.Fatal(err)
	}
	ticket := transition.Ticket()
	if err := transition.Rollback(context.Background()); err != nil {
		_ = transition.Close()
		t.Fatal(err)
	}
	defer transition.Close()
	if transition.State() != CredentialEndpointMaintenanceRolledBack {
		t.Fatalf("state = %q, want rolled_back", transition.State())
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("restored socket = %#v, %v", info, err)
	}
	actual, err := InspectLegacyCredentialEndpointTransition(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if actual.State != CredentialEndpointMaintenanceRolledBack || actual.Ticket != ticket {
		t.Fatalf("pending status = %#v, want rolled_back ticket", actual)
	}
	endpoint, client, openErr := openCredentialEndpoint(path, true, nil)
	if endpoint != nil {
		_ = endpoint.Close()
	}
	if client != nil {
		_ = client.Close()
	}
	if !errors.Is(openErr, ErrCredentialEndpointMaintenancePending) {
		t.Fatalf("recovering open error = %v, want maintenance pending", openErr)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(path), ticket.QuarantineName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback quarantine error = %v, want absent", err)
	}
	for _, retained := range []string{credentialEndpointMaintenanceJournalPath(path), credentialEndpointLockPath(path)} {
		if _, err := os.Lstat(retained); err != nil {
			t.Fatalf("retained path %s error = %v", retained, err)
		}
	}
}

func TestResumeLegacyCredentialEndpointTransitionReusesTicketAndRedetachesRolledBackSocket(t *testing.T) {
	t.Parallel()
	path := createRefusedLegacyCredentialSocket(t)
	snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	authority := DrainAuthorityFunc(func(context.Context, string) error { return nil })
	transition, err := PrepareLegacyCredentialEndpointTransition(context.Background(), path, snapshot, authority)
	if err != nil {
		t.Fatal(err)
	}
	ticket := transition.Ticket()
	if err := transition.Rollback(context.Background()); err != nil {
		_ = transition.Close()
		t.Fatal(err)
	}
	if err := transition.Close(); err != nil {
		t.Fatal(err)
	}
	assertions := 0
	resumeAuthority := DrainAuthorityFunc(func(context.Context, string) error {
		assertions++
		return nil
	})
	resumed, err := ResumeLegacyCredentialEndpointTransition(context.Background(), path, ticket, resumeAuthority)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if resumed.Ticket() != ticket || resumed.State() != CredentialEndpointMaintenanceQuarantined {
		t.Fatalf("resumed = ticket %#v state %q", resumed.Ticket(), resumed.State())
	}
	if assertions < 5 {
		t.Fatalf("resume drain assertions = %d, want fresh probes and mutation checks", assertions)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("re-detached canonical error = %v, want absent", err)
	}
}

func TestPrepareLegacyCredentialEndpointTransitionAdoptsPreJournalOrphanLock(t *testing.T) {
	t.Parallel()
	path := createRefusedLegacyCredentialSocket(t)
	snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	fsys := fsutil.OSFileSystem{}
	directory, err := fsys.OpenSecureDirectory(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	lock, err := fsutil.AcquireNewExclusiveLockInDirectory(fsys, directory, filepath.Base(credentialEndpointLockPath(path)))
	if err != nil {
		_ = directory.Close()
		t.Fatal(err)
	}
	lockInfo, err := lock.Stat()
	if err != nil {
		t.Fatal(err)
	}
	lockIdentity, ok := fsys.FileIdentity(lockInfo)
	if !ok {
		t.Fatal("orphan lock identity unavailable")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := directory.Close(); err != nil {
		t.Fatal(err)
	}

	transition, err := PrepareLegacyCredentialEndpointTransition(context.Background(), path, snapshot, DrainAuthorityFunc(func(context.Context, string) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	defer transition.Close()
	ticket := transition.Ticket()
	if ticket.Lock.Device != lockIdentity.Device || ticket.Lock.Inode != lockIdentity.Inode {
		t.Fatalf("adopted lock = %#v, want device/inode %#v", ticket.Lock, lockIdentity)
	}
}

func TestLegacyCredentialEndpointTransitionHasOneOwnerAndCloseChangesNoNamespace(t *testing.T) {
	t.Parallel()
	path := createRefusedLegacyCredentialSocket(t)
	snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	authority := DrainAuthorityFunc(func(context.Context, string) error { return nil })
	transition, err := PrepareLegacyCredentialEndpointTransition(context.Background(), path, snapshot, authority)
	if err != nil {
		t.Fatal(err)
	}
	ticket := transition.Ticket()
	before := maintenanceDirectoryInventory(t, filepath.Dir(path))
	contender, err := ResumeLegacyCredentialEndpointTransition(context.Background(), path, ticket, authority)
	if contender != nil {
		_ = contender.Close()
		t.Fatal("concurrent resume acquired a second owner")
	}
	if !errors.Is(err, fsutil.ErrExclusiveLockHeld) {
		t.Fatalf("concurrent resume error = %v, want lock held", err)
	}
	if after := maintenanceDirectoryInventory(t, filepath.Dir(path)); !reflect.DeepEqual(after, before) {
		t.Fatalf("contended resume changed namespace: before=%v after=%v", before, after)
	}
	if err := transition.Close(); err != nil {
		t.Fatal(err)
	}
	if after := maintenanceDirectoryInventory(t, filepath.Dir(path)); !reflect.DeepEqual(after, before) {
		t.Fatalf("Close changed namespace: before=%v after=%v", before, after)
	}
	resumed, err := ResumeLegacyCredentialEndpointTransition(context.Background(), path, ticket, authority)
	if err != nil {
		t.Fatal(err)
	}
	_ = resumed.Close()
}

func TestResumeLegacyCredentialEndpointTransitionAcceptsOnlyAttributedCrashShapes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		state     CredentialEndpointMaintenanceState
		shape     string
		wantState CredentialEndpointMaintenanceState
	}{
		{name: "prepared final only", state: CredentialEndpointMaintenancePrepared, shape: "final", wantState: CredentialEndpointMaintenanceQuarantined},
		{name: "prepared dual", state: CredentialEndpointMaintenancePrepared, shape: "dual", wantState: CredentialEndpointMaintenanceQuarantined},
		{name: "prepared quarantine only", state: CredentialEndpointMaintenancePrepared, shape: "quarantine", wantState: CredentialEndpointMaintenanceQuarantined},
		{name: "committing quarantine only", state: CredentialEndpointMaintenanceCommitting, shape: "quarantine", wantState: CredentialEndpointMaintenanceCommitted},
		{name: "committing neither", state: CredentialEndpointMaintenanceCommitting, shape: "neither", wantState: CredentialEndpointMaintenanceCommitted},
		{name: "rolling back quarantine only", state: CredentialEndpointMaintenanceRollingBack, shape: "quarantine", wantState: CredentialEndpointMaintenanceRolledBack},
		{name: "rolling back dual", state: CredentialEndpointMaintenanceRollingBack, shape: "dual", wantState: CredentialEndpointMaintenanceRolledBack},
		{name: "rolling back final only", state: CredentialEndpointMaintenanceRollingBack, shape: "final", wantState: CredentialEndpointMaintenanceRolledBack},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := createRefusedLegacyCredentialSocket(t)
			snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			authority := DrainAuthorityFunc(func(context.Context, string) error { return nil })
			transition, err := PrepareLegacyCredentialEndpointTransition(context.Background(), path, snapshot, authority)
			if err != nil {
				t.Fatal(err)
			}
			ticket := transition.Ticket()
			if test.shape == "final" && test.state == CredentialEndpointMaintenancePrepared {
				if err := transition.Rollback(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if err := transition.Close(); err != nil {
				t.Fatal(err)
			}
			quarantinePath := filepath.Join(filepath.Dir(path), ticket.QuarantineName)
			switch test.shape {
			case "dual":
				if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
					if err := os.Link(quarantinePath, path); err != nil {
						t.Fatal(err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			case "final":
				if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
					if err := os.Link(quarantinePath, path); err != nil {
						t.Fatal(err)
					}
					if err := os.Remove(quarantinePath); err != nil {
						t.Fatal(err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			case "neither":
				if err := os.Remove(quarantinePath); err != nil {
					t.Fatal(err)
				}
			}
			rewriteMaintenanceJournalStateForTest(t, path, test.state)
			resumed, err := ResumeLegacyCredentialEndpointTransition(context.Background(), path, ticket, authority)
			if err != nil {
				t.Fatal(err)
			}
			defer resumed.Close()
			if resumed.State() != test.wantState {
				t.Fatalf("resumed state = %q, want %q", resumed.State(), test.wantState)
			}
		})
	}
}

func TestPrepareLegacyCredentialEndpointTransitionRejectsSourceReplacementBeforeLink(t *testing.T) {
	t.Parallel()
	path := createRefusedLegacyCredentialSocket(t)
	snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	assertions := 0
	authority := DrainAuthorityFunc(func(context.Context, string) error {
		assertions++
		if assertions != 5 {
			return nil
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			return err
		}
		listener.SetUnlinkOnClose(false)
		if err := os.Chmod(path, 0o600); err != nil {
			_ = listener.Close()
			return err
		}
		return listener.Close()
	})
	transition, err := PrepareLegacyCredentialEndpointTransition(context.Background(), path, snapshot, authority)
	if transition != nil {
		_ = transition.Close()
		t.Fatal("source replacement returned a live transition")
	}
	if !errors.Is(err, ErrCredentialEndpointIdentityChanged) {
		t.Fatalf("prepare error = %v, want identity changed", err)
	}
	journalData, err := os.ReadFile(credentialEndpointMaintenanceJournalPath(path))
	if err != nil {
		t.Fatal(err)
	}
	journal, err := decodeCredentialEndpointMaintenanceJournal(journalData)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(path), journal.Ticket.QuarantineName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement was linked into quarantine: %v", err)
	}
	current, err := InspectLegacyCredentialEndpointTransition(context.Background(), path)
	if err == nil || current.State != "" {
		t.Fatalf("mismatched prepared status = %#v, %v; want conflict", current, err)
	}
}

func rewriteMaintenanceJournalStateForTest(t *testing.T, path string, state CredentialEndpointMaintenanceState) {
	t.Helper()
	journalPath := credentialEndpointMaintenanceJournalPath(path)
	data, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := decodeCredentialEndpointMaintenanceJournal(data)
	if err != nil {
		t.Fatal(err)
	}
	journal.Generation++
	journal.State = state
	data, err = encodeCredentialEndpointMaintenanceJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journalPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func maintenanceDirectoryInventory(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	inventory := make([]string, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		inventory = append(inventory, entry.Name()+":"+info.Mode().String())
	}
	return inventory
}

func createRefusedLegacyCredentialSocket(t *testing.T) string {
	t.Helper()
	dir := shortEndpointDir(t)
	path := filepath.Join(dir, "credential.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	return path
}
