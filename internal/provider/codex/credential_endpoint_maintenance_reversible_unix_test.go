//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestCredentialEndpointMaintenanceJournalAcceptsReversibleStates(t *testing.T) {
	t.Parallel()
	path := createRefusedLegacyCredentialSocket(t)
	snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	transition, err := PrepareLegacyCredentialEndpointTransition(
		context.Background(),
		path,
		snapshot,
		DrainAuthorityFunc(func(context.Context, string) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer transition.Close()

	data, err := os.ReadFile(credentialEndpointMaintenanceJournalPath(path))
	if err != nil {
		t.Fatal(err)
	}
	journal, err := decodeCredentialEndpointMaintenanceJournal(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []CredentialEndpointMaintenanceState{"activating", "activated", "finalising"} {
		candidate := journal
		candidate.Generation++
		candidate.State = state
		if state == CredentialEndpointMaintenanceFinalising {
			candidate.Owner = &credentialEndpointMaintenanceOwnerProof{
				Generation:    strings.Repeat("a", 32),
				Socket:        journal.Ticket.Socket,
				SidecarFile:   journal.Ticket.Lock,
				SidecarSHA256: strings.Repeat("b", 64),
			}
		}
		encoded, err := encodeCredentialEndpointMaintenanceJournal(candidate)
		if err != nil {
			t.Fatalf("encode state %q: %v", state, err)
		}
		decoded, err := decodeCredentialEndpointMaintenanceJournal(encoded)
		if err != nil {
			t.Fatalf("decode state %q: %v", state, err)
		}
		if !reflect.DeepEqual(decoded, candidate) {
			t.Fatalf("decoded state %q = %#v, want %#v", state, decoded, candidate)
		}
	}
}

func TestCredentialEndpointMaintenanceRecordRequiresStrictOwnerField(t *testing.T) {
	t.Parallel()
	path := createRefusedLegacyCredentialSocket(t)
	snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	transition, err := PrepareLegacyCredentialEndpointTransition(
		context.Background(), path, snapshot,
		DrainAuthorityFunc(func(context.Context, string) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer transition.Close()

	data, err := os.ReadFile(credentialEndpointMaintenanceJournalPath(path))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if owner, exists := fields["owner"]; !exists || string(owner) != "null" {
		t.Fatalf("prepared owner field = %s, exists=%t; want required null", owner, exists)
	}

	delete(fields, "owner")
	missingOwner, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeCredentialEndpointMaintenanceJournal(missingOwner); err == nil {
		t.Fatal("record without owner field decoded successfully")
	}

	journal, err := decodeCredentialEndpointMaintenanceJournal(data)
	if err != nil {
		t.Fatal(err)
	}
	socket, err := json.Marshal(journal.Ticket.Socket)
	if err != nil {
		t.Fatal(err)
	}
	sidecarFile, err := json.Marshal(journal.Ticket.Lock)
	if err != nil {
		t.Fatal(err)
	}
	fields["state"] = json.RawMessage(`"activated"`)
	fields["generation"] = json.RawMessage(`3`)
	fields["owner"] = json.RawMessage(fmt.Sprintf(
		`{"generation":"%s","socket":%s,"sidecar_file":%s,"sidecar_sha256":"%s"}`,
		strings.Repeat("a", 32), socket, sidecarFile, strings.Repeat("b", 64),
	))
	boundOwner, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeCredentialEndpointMaintenanceJournal(boundOwner); err != nil {
		t.Fatalf("strict bound-owner record rejected: %v", err)
	}

	fields["owner"] = append(fields["owner"][:len(fields["owner"])-1], []byte(`,"unknown":true}`)...)
	unknownOwner, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeCredentialEndpointMaintenanceJournal(unknownOwner); err == nil {
		t.Fatal("record with unknown owner field decoded successfully")
	}
}

func TestActivateLegacyCredentialEndpointTransitionRetainsRollbackRecord(t *testing.T) {
	t.Parallel()
	path := createRefusedLegacyCredentialSocket(t)
	snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	transition, err := PrepareLegacyCredentialEndpointTransition(
		context.Background(), path, snapshot,
		DrainAuthorityFunc(func(context.Context, string) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	ticket := transition.Ticket()
	if err := transition.Activate(context.Background()); err != nil {
		_ = transition.Close()
		t.Fatalf("Activate error = %v", err)
	}
	if got := transition.State(); got != CredentialEndpointMaintenanceActivated {
		_ = transition.Close()
		t.Fatalf("state = %q, want activated", got)
	}
	if err := transition.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Lstat(credentialEndpointMaintenanceJournalPath(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal still exists: %v", err)
	}
	data, err := os.ReadFile(credentialEndpointMaintenanceRollbackPath(path))
	if err != nil {
		t.Fatal(err)
	}
	record, err := decodeCredentialEndpointMaintenanceJournal(data)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != CredentialEndpointMaintenanceActivated || record.Ticket != ticket || record.Owner != nil {
		t.Fatalf("rollback record = %#v, want activated exact ticket without owner", record)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canonical endpoint exists after activation: %v", err)
	}
	quarantine := filepath.Join(filepath.Dir(path), ticket.QuarantineName)
	info, err := os.Lstat(quarantine)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("quarantine stat has unexpected type")
	}
	proof := LegacyCredentialEndpointIdentity{
		Device: uint64(stat.Dev), Inode: uint64(stat.Ino), UID: uint64(stat.Uid), Links: uint64(stat.Nlink),
		Type: "socket", Mode: uint32(stat.Mode) & 0o777,
	}
	if proof != ticket.Socket {
		t.Fatalf("quarantine proof = %#v, want %#v", proof, ticket.Socket)
	}
	if info, err := os.Lstat(credentialEndpointLockPath(path)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("permanent lock = %v, %v", info, err)
	}
}

func TestResumeLegacyCredentialEndpointActivationCrashShapes(t *testing.T) {
	for _, withRollback := range []bool{false, true} {
		withRollback := withRollback
		t.Run(fmt.Sprintf("rollback_record_%t", withRollback), func(t *testing.T) {
			t.Parallel()
			path := createRefusedLegacyCredentialSocket(t)
			snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			transition, err := PrepareLegacyCredentialEndpointTransition(
				context.Background(), path, snapshot,
				DrainAuthorityFunc(func(context.Context, string) error { return nil }),
			)
			if err != nil {
				t.Fatal(err)
			}
			ticket := transition.Ticket()
			if err := transition.Close(); err != nil {
				t.Fatal(err)
			}

			data, err := os.ReadFile(credentialEndpointMaintenanceJournalPath(path))
			if err != nil {
				t.Fatal(err)
			}
			journal, err := decodeCredentialEndpointMaintenanceJournal(data)
			if err != nil {
				t.Fatal(err)
			}
			journal.Generation++
			journal.State = CredentialEndpointMaintenanceActivating
			encoded, err := encodeCredentialEndpointMaintenanceJournal(journal)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(credentialEndpointMaintenanceJournalPath(path), encoded, 0o600); err != nil {
				t.Fatal(err)
			}
			if withRollback {
				record := journal
				record.Generation++
				record.State = CredentialEndpointMaintenanceActivated
				recordData, err := encodeCredentialEndpointMaintenanceJournal(record)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(credentialEndpointMaintenanceRollbackPath(path), recordData, 0o600); err != nil {
					t.Fatal(err)
				}
			}

			status, err := InspectLegacyCredentialEndpointTransition(context.Background(), path)
			if err != nil {
				t.Fatalf("InspectTransition error = %v", err)
			}
			if status.State != CredentialEndpointMaintenanceActivating || status.Ticket != ticket {
				t.Fatalf("status = %#v, want activating exact ticket", status)
			}
			resumed, err := ResumeLegacyCredentialEndpointTransition(
				context.Background(), path, ticket,
				DrainAuthorityFunc(func(context.Context, string) error { return nil }),
			)
			if err != nil {
				t.Fatalf("Resume error = %v", err)
			}
			if resumed.State() != CredentialEndpointMaintenanceActivated {
				_ = resumed.Close()
				t.Fatalf("resumed state = %q, want activated", resumed.State())
			}
			if err := resumed.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(credentialEndpointMaintenanceJournalPath(path)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("journal remains after resume: %v", err)
			}
			status, err = InspectLegacyCredentialEndpointTransition(context.Background(), path)
			if err != nil || status.State != CredentialEndpointMaintenanceActivated || status.Ticket != ticket {
				t.Fatalf("activated status = %#v, %v", status, err)
			}
		})
	}
}

func TestActivateLegacyCredentialEndpointTransitionIsIdempotentAfterJournalRemoval(t *testing.T) {
	t.Parallel()
	path := createRefusedLegacyCredentialSocket(t)
	snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	transition, err := PrepareLegacyCredentialEndpointTransition(
		context.Background(), path, snapshot,
		DrainAuthorityFunc(func(context.Context, string) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	ticket := transition.Ticket()
	if err := transition.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := transition.Close(); err != nil {
		t.Fatal(err)
	}
	recordBefore, err := os.ReadFile(credentialEndpointMaintenanceRollbackPath(path))
	if err != nil {
		t.Fatal(err)
	}

	resumed, err := ResumeLegacyCredentialEndpointTransition(
		context.Background(), path, ticket,
		DrainAuthorityFunc(func(context.Context, string) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := resumed.Activate(context.Background()); err != nil {
		_ = resumed.Close()
		t.Fatalf("idempotent Activate error = %v", err)
	}
	if err := resumed.Close(); err != nil {
		t.Fatal(err)
	}
	recordAfter, err := os.ReadFile(credentialEndpointMaintenanceRollbackPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(recordAfter, recordBefore) {
		t.Fatal("idempotent Activate rewrote rollback record")
	}
}

func TestActivatedMaintenanceReceiptBindsCandidateBeforeAcceptAndSurvivesClose(t *testing.T) {
	t.Parallel()
	path := createRefusedLegacyCredentialSocket(t)
	snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	transition, err := PrepareLegacyCredentialEndpointTransition(
		context.Background(), path, snapshot,
		DrainAuthorityFunc(func(context.Context, string) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	ticket := transition.Ticket()
	if err := transition.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := transition.Close(); err != nil {
		t.Fatal(err)
	}

	coordinator, _ := testCoordinator(t)
	var acceptedRecord credentialEndpointMaintenanceJournal
	owner, err := openCredentialControlPrepared(context.Background(), path, coordinator, false, nil, nil, func() {
		data, readErr := os.ReadFile(credentialEndpointMaintenanceRollbackPath(path))
		if readErr != nil {
			t.Errorf("read receipt before accept: %v", readErr)
			return
		}
		acceptedRecord, readErr = decodeCredentialEndpointMaintenanceJournal(data)
		if readErr != nil {
			t.Errorf("decode receipt before accept: %v", readErr)
		}
	})
	if err != nil {
		t.Fatalf("candidate open error = %v", err)
	}
	if !owner.Owner() {
		_ = owner.Close()
		t.Fatal("candidate did not become owner")
	}
	if acceptedRecord.State != CredentialEndpointMaintenanceActivated || acceptedRecord.Ticket != ticket || acceptedRecord.Owner == nil {
		_ = owner.Close()
		t.Fatalf("receipt before accept = %#v, want bound activated receipt", acceptedRecord)
	}
	if acceptedRecord.Owner.Generation == "" {
		_ = owner.Close()
		t.Fatal("bound receipt has empty owner generation")
	}

	delegate, err := OpenCredentialControl(path, coordinator)
	if err != nil {
		_ = owner.Close()
		t.Fatalf("delegate open error = %v", err)
	}
	if delegate.Owner() {
		_ = delegate.Close()
		_ = owner.Close()
		t.Fatal("second candidate became owner")
	}
	if err := delegate.Close(); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}

	for _, retained := range []string{
		credentialEndpointMaintenanceRollbackPath(path),
		credentialEndpointLockPath(path),
		filepath.Join(filepath.Dir(path), ticket.QuarantineName),
	} {
		if _, err := os.Lstat(retained); err != nil {
			t.Fatalf("retained artifact %s: %v", filepath.Base(retained), err)
		}
	}
	for _, absent := range []string{path, credentialEndpointSidecarPath(path), credentialEndpointMaintenanceJournalPath(path)} {
		if _, err := os.Lstat(absent); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("artifact %s remains: %v", filepath.Base(absent), err)
		}
	}
}

func TestBoundActivatedMaintenanceReceiptCannotBeReplayedAfterOwnerClose(t *testing.T) {
	for _, allowRecovery := range []bool{false, true} {
		allowRecovery := allowRecovery
		t.Run(fmt.Sprintf("recovering_%t", allowRecovery), func(t *testing.T) {
			t.Parallel()
			path := createRefusedLegacyCredentialSocket(t)
			snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			transition, err := PrepareLegacyCredentialEndpointTransition(
				context.Background(), path, snapshot,
				DrainAuthorityFunc(func(context.Context, string) error { return nil }),
			)
			if err != nil {
				t.Fatal(err)
			}
			ticket := transition.Ticket()
			if err := transition.Activate(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := transition.Close(); err != nil {
				t.Fatal(err)
			}
			coordinator, _ := testCoordinator(t)
			owner, err := openCredentialControlPrepared(context.Background(), path, coordinator, false, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := owner.Close(); err != nil {
				t.Fatal(err)
			}

			receiptPath := credentialEndpointMaintenanceRollbackPath(path)
			receiptBefore, err := os.ReadFile(receiptPath)
			if err != nil {
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
			receiptAfter, err := os.ReadFile(receiptPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(receiptAfter, receiptBefore) {
				t.Fatal("failed-closed opener rewrote the bound activated receipt")
			}
			for _, absent := range []string{path, credentialEndpointSidecarPath(path)} {
				if _, err := os.Lstat(absent); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("failed-closed opener republished %s: %v", filepath.Base(absent), err)
				}
			}
			if _, err := os.Lstat(filepath.Join(filepath.Dir(path), ticket.QuarantineName)); err != nil {
				t.Fatalf("failed-closed opener removed quarantine: %v", err)
			}
		})
	}
}

func TestBoundActivatedMaintenanceDeadOwnerResidueIsRollbackOnly(t *testing.T) {
	for _, allowRecovery := range []bool{false, true} {
		allowRecovery := allowRecovery
		t.Run(fmt.Sprintf("recovering_%t", allowRecovery), func(t *testing.T) {
			t.Parallel()
			path := createRefusedLegacyCredentialSocket(t)
			snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			transition, err := PrepareLegacyCredentialEndpointTransition(
				context.Background(), path, snapshot,
				DrainAuthorityFunc(func(context.Context, string) error { return nil }),
			)
			if err != nil {
				t.Fatal(err)
			}
			ticket := transition.Ticket()
			if err := transition.Activate(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := transition.Close(); err != nil {
				t.Fatal(err)
			}
			endpoint, client, err := openCredentialEndpoint(path, false, nil)
			if err != nil || client != nil || endpoint == nil {
				t.Fatalf("candidate endpoint = %v, %v, %v", endpoint, client, err)
			}
			if err := endpoint.listener.Close(); err != nil {
				t.Fatal(err)
			}
			endpoint.release()

			paths := []string{
				credentialEndpointMaintenanceRollbackPath(path),
				path,
				credentialEndpointSidecarPath(path),
				filepath.Join(filepath.Dir(path), ticket.QuarantineName),
			}
			type artifactProof struct {
				identity fsutil.SecureFileIdentity
				data     []byte
			}
			before := make(map[string]artifactProof, len(paths))
			for _, artifact := range paths {
				info, err := os.Lstat(artifact)
				if err != nil {
					t.Fatal(err)
				}
				identity, ok := (fsutil.OSFileSystem{}).FileIdentity(info)
				if !ok {
					t.Fatalf("identity unavailable for %s", filepath.Base(artifact))
				}
				var data []byte
				if info.Mode().IsRegular() {
					data, err = os.ReadFile(artifact)
					if err != nil {
						t.Fatal(err)
					}
				}
				before[artifact] = artifactProof{identity: identity, data: data}
			}
			opened, delegated, err := openCredentialEndpoint(path, allowRecovery, nil)
			if opened != nil {
				_ = opened.Close()
			}
			if delegated != nil {
				_ = delegated.Close()
			}
			if !errors.Is(err, ErrCredentialEndpointMaintenancePending) {
				t.Fatalf("open error = %v, want maintenance pending", err)
			}
			for _, artifact := range paths {
				info, err := os.Lstat(artifact)
				if err != nil {
					t.Fatalf("failed-closed opener changed %s: %v", filepath.Base(artifact), err)
				}
				identity, ok := (fsutil.OSFileSystem{}).FileIdentity(info)
				if !ok || identity != before[artifact].identity {
					t.Fatalf("failed-closed opener replaced %s", filepath.Base(artifact))
				}
				if info.Mode().IsRegular() {
					after, err := os.ReadFile(artifact)
					if err != nil || !bytes.Equal(after, before[artifact].data) {
						t.Fatalf("failed-closed opener rewrote %s: %v", filepath.Base(artifact), err)
					}
				}
			}
		})
	}
}

func TestBoundActivatedMaintenanceInspectAndResumeRejectMismatchedOwnerProof(t *testing.T) {
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
	if err := transition.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := transition.Close(); err != nil {
		t.Fatal(err)
	}
	endpoint, client, err := openCredentialEndpoint(path, false, nil)
	if err != nil || client != nil || endpoint == nil {
		t.Fatalf("candidate endpoint = %v, %v, %v", endpoint, client, err)
	}
	if err := endpoint.listener.Close(); err != nil {
		t.Fatal(err)
	}
	endpoint.release()

	receiptPath := credentialEndpointMaintenanceRollbackPath(path)
	data, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	record, err := decodeCredentialEndpointMaintenanceJournal(data)
	if err != nil || record.Owner == nil {
		t.Fatalf("bound receipt = %#v, %v", record, err)
	}
	record.Owner.Socket.Inode++
	data, err = encodeCredentialEndpointMaintenanceJournal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(receiptPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectLegacyCredentialEndpointTransition(context.Background(), path); !errors.Is(err, ErrCredentialEndpointIdentityChanged) {
		t.Fatalf("Inspect error = %v, want identity changed", err)
	}
	resumed, err := ResumeLegacyCredentialEndpointTransition(context.Background(), path, ticket, authority)
	if resumed != nil {
		_ = resumed.Close()
		t.Fatal("Resume returned a transition for mismatched owner proof")
	}
	if !errors.Is(err, ErrCredentialEndpointIdentityChanged) {
		t.Fatalf("Resume error = %v, want identity changed", err)
	}
	for _, retained := range []string{
		receiptPath,
		path,
		credentialEndpointSidecarPath(path),
		filepath.Join(filepath.Dir(path), ticket.QuarantineName),
	} {
		if _, err := os.Lstat(retained); err != nil {
			t.Fatalf("failed validation removed %s: %v", filepath.Base(retained), err)
		}
	}
}

func TestActivatedMaintenanceReceiptReplacementBeforePublicationFailsClosed(t *testing.T) {
	t.Parallel()
	path := createRefusedLegacyCredentialSocket(t)
	snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	transition, err := PrepareLegacyCredentialEndpointTransition(
		context.Background(), path, snapshot,
		DrainAuthorityFunc(func(context.Context, string) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	ticket := transition.Ticket()
	if err := transition.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := transition.Close(); err != nil {
		t.Fatal(err)
	}
	receiptPath := credentialEndpointMaintenanceRollbackPath(path)
	receipt, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	var replaced sync.Once
	hook := func(phase credentialEndpointPhase) {
		if phase != credentialEndpointPhaseMaintenanceAdmitted {
			return
		}
		replaced.Do(func() {
			temporary := filepath.Join(filepath.Dir(path), ".replacement-receipt")
			if err := os.WriteFile(temporary, receipt, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(temporary, receiptPath); err != nil {
				t.Fatal(err)
			}
		})
	}
	coordinator, _ := testCoordinator(t)
	control, err := openCredentialControlPrepared(context.Background(), path, coordinator, false, hook, nil, nil)
	if control != nil {
		_ = control.Close()
	}
	if !errors.Is(err, ErrCredentialEndpointMaintenancePending) {
		t.Fatalf("open error = %v, want maintenance pending", err)
	}
	after, err := os.Lstat(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeIdentity, beforeOK := (fsutil.OSFileSystem{}).FileIdentity(before)
	afterIdentity, afterOK := (fsutil.OSFileSystem{}).FileIdentity(after)
	if !beforeOK || !afterOK || beforeIdentity == afterIdentity {
		t.Fatal("test did not replace the activated receipt inode")
	}
	for _, absent := range []string{path, credentialEndpointSidecarPath(path)} {
		if _, err := os.Lstat(absent); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed-closed opener published %s: %v", filepath.Base(absent), err)
		}
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(path), ticket.QuarantineName)); err != nil {
		t.Fatalf("failed-closed opener removed quarantine: %v", err)
	}
}

func TestActivatedMaintenanceReceiptReplacementAfterPublicationPreservesUnboundOwnerResidue(t *testing.T) {
	t.Parallel()
	path := createRefusedLegacyCredentialSocket(t)
	snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	transition, err := PrepareLegacyCredentialEndpointTransition(
		context.Background(), path, snapshot,
		DrainAuthorityFunc(func(context.Context, string) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	ticket := transition.Ticket()
	if err := transition.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := transition.Close(); err != nil {
		t.Fatal(err)
	}
	receiptPath := credentialEndpointMaintenanceRollbackPath(path)
	receipt, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	var replaced sync.Once
	hook := func(phase credentialEndpointPhase) {
		if phase != credentialEndpointPhasePublished {
			return
		}
		replaced.Do(func() {
			temporary := filepath.Join(filepath.Dir(path), ".published-replacement-receipt")
			if err := os.WriteFile(temporary, receipt, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(temporary, receiptPath); err != nil {
				t.Fatal(err)
			}
		})
	}

	endpoint, client, err := openCredentialEndpoint(path, false, hook)
	if endpoint != nil {
		_ = endpoint.Close()
	}
	if client != nil {
		_ = client.Close()
	}
	if !errors.Is(err, ErrCredentialEndpointMaintenancePending) {
		t.Fatalf("open error = %v, want maintenance pending", err)
	}
	after, err := os.Lstat(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeIdentity, beforeOK := (fsutil.OSFileSystem{}).FileIdentity(before)
	afterIdentity, afterOK := (fsutil.OSFileSystem{}).FileIdentity(after)
	if !beforeOK || !afterOK || beforeIdentity == afterIdentity {
		t.Fatal("test did not replace the activated receipt inode")
	}
	data, err := os.ReadFile(receiptPath)
	if err != nil || !bytes.Equal(data, receipt) {
		t.Fatalf("replacement receipt changed: %v", err)
	}
	for _, retained := range []string{
		path,
		credentialEndpointSidecarPath(path),
		filepath.Join(filepath.Dir(path), ticket.QuarantineName),
	} {
		if _, err := os.Lstat(retained); err != nil {
			t.Fatalf("ambiguous publication removed %s: %v", filepath.Base(retained), err)
		}
	}
}

func TestConcurrentActivatedMaintenanceOpenersElectOnePublisher(t *testing.T) {
	path := createRefusedLegacyCredentialSocket(t)
	snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	transition, err := PrepareLegacyCredentialEndpointTransition(
		context.Background(), path, snapshot,
		DrainAuthorityFunc(func(context.Context, string) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := transition.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := transition.Close(); err != nil {
		t.Fatal(err)
	}
	coordinator, _ := testCoordinator(t)
	const contenders = 4
	start := make(chan struct{})
	type result struct {
		control *CredentialControl
		err     error
	}
	results := make(chan result, contenders)
	for range contenders {
		go func() {
			<-start
			deadline := time.Now().Add(2 * time.Second)
			for {
				control, err := openCredentialControlPrepared(context.Background(), path, coordinator, false, nil, nil, nil)
				if !errors.Is(err, ErrCredentialEndpointMaintenancePending) || time.Now().After(deadline) {
					results <- result{control: control, err: err}
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}
	close(start)
	controls := make([]*CredentialControl, 0, contenders)
	owners := 0
	for range contenders {
		result := <-results
		if result.err != nil {
			for _, control := range controls {
				_ = control.Close()
			}
			t.Fatalf("concurrent open error = %v", result.err)
		}
		controls = append(controls, result.control)
		if result.control.Owner() {
			owners++
		}
	}
	if owners != 1 {
		for _, control := range controls {
			_ = control.Close()
		}
		t.Fatalf("owner count = %d, want 1", owners)
	}
	for _, control := range controls {
		if !control.Owner() {
			if err := control.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, control := range controls {
		if control.Owner() {
			if err := control.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestActivatedMaintenanceUnboundPartialPublicationIsPreserved(t *testing.T) {
	t.Parallel()
	path := createRefusedLegacyCredentialSocket(t)
	snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	transition, err := PrepareLegacyCredentialEndpointTransition(
		context.Background(), path, snapshot,
		DrainAuthorityFunc(func(context.Context, string) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	ticket := transition.Ticket()
	if err := transition.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := transition.Close(); err != nil {
		t.Fatal(err)
	}

	receiptPath := credentialEndpointMaintenanceRollbackPath(path)
	receiptBefore, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	partialBefore, err := os.Lstat(path)
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	endpoint, client, err := openCredentialEndpoint(path, false, nil)
	if endpoint != nil {
		_ = endpoint.Close()
	}
	if client != nil {
		_ = client.Close()
	}
	if !errors.Is(err, ErrCredentialEndpointMaintenancePending) {
		t.Fatalf("open error = %v, want maintenance pending", err)
	}
	partialAfter, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeIdentity, beforeOK := (fsutil.OSFileSystem{}).FileIdentity(partialBefore)
	afterIdentity, afterOK := (fsutil.OSFileSystem{}).FileIdentity(partialAfter)
	if !beforeOK || !afterOK || beforeIdentity != afterIdentity {
		t.Fatal("failed-closed opener replaced the unbound partial socket")
	}
	receiptAfter, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(receiptAfter, receiptBefore) {
		t.Fatal("failed-closed opener rewrote the unbound activated receipt")
	}
	if _, err := os.Lstat(credentialEndpointSidecarPath(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed-closed opener created a sidecar: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(path), ticket.QuarantineName)); err != nil {
		t.Fatalf("failed-closed opener removed quarantine: %v", err)
	}
}

func TestActivatedMaintenanceUnboundPartialSidecarIsPreserved(t *testing.T) {
	t.Parallel()
	path := createRefusedLegacyCredentialSocket(t)
	snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	transition, err := PrepareLegacyCredentialEndpointTransition(
		context.Background(), path, snapshot,
		DrainAuthorityFunc(func(context.Context, string) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	ticket := transition.Ticket()
	if err := transition.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := transition.Close(); err != nil {
		t.Fatal(err)
	}

	receiptPath := credentialEndpointMaintenanceRollbackPath(path)
	receiptBefore, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	sidecarPath := credentialEndpointSidecarPath(path)
	partial := []byte("partial-owner-proof")
	if err := os.WriteFile(sidecarPath, partial, 0o600); err != nil {
		t.Fatal(err)
	}

	endpoint, client, err := openCredentialEndpoint(path, false, nil)
	if endpoint != nil {
		_ = endpoint.Close()
	}
	if client != nil {
		_ = client.Close()
	}
	if !errors.Is(err, ErrCredentialEndpointMaintenancePending) {
		t.Fatalf("open error = %v, want maintenance pending", err)
	}
	partialAfter, err := os.ReadFile(sidecarPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(partialAfter, partial) {
		t.Fatal("failed-closed opener rewrote the unbound partial sidecar")
	}
	receiptAfter, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(receiptAfter, receiptBefore) {
		t.Fatal("failed-closed opener rewrote the unbound activated receipt")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed-closed opener published a socket: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(path), ticket.QuarantineName)); err != nil {
		t.Fatalf("failed-closed opener removed quarantine: %v", err)
	}
}

func TestFinaliseLegacyCredentialEndpointTransitionRunsVerifierInsideLiveOwner(t *testing.T) {
	t.Parallel()
	path := createRefusedLegacyCredentialSocket(t)
	snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	transition, err := PrepareLegacyCredentialEndpointTransition(
		context.Background(), path, snapshot,
		DrainAuthorityFunc(func(context.Context, string) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	ticket := transition.Ticket()
	if err := transition.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := transition.Close(); err != nil {
		t.Fatal(err)
	}

	coordinator, _ := testCoordinator(t)
	var verified atomic.Int32
	verifier := testLegacyMaintenanceFinaliseVerifier(func(_ context.Context, proof LegacyMaintenanceFinaliseVerification) error {
		if proof.TicketHash == "" || proof.OwnerGeneration == "" {
			return errors.New("unbound verification proof")
		}
		verified.Add(1)
		return nil
	})
	owner, err := openCredentialControlPreparedWithLegacyMaintenanceVerifier(
		context.Background(), path, coordinator, false, nil, nil, nil, verifier,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := FinaliseLegacyCredentialEndpointTransition(context.Background(), path, ticket); err != nil {
		_ = owner.Close()
		t.Fatalf("Finalise error = %v", err)
	}
	if got := verified.Load(); got != 1 {
		_ = owner.Close()
		t.Fatalf("verifier calls = %d, want 1", got)
	}
	if err := FinaliseLegacyCredentialEndpointTransition(context.Background(), path, ticket); err != nil {
		_ = owner.Close()
		t.Fatalf("idempotent Finalise error = %v", err)
	}
	if got := verified.Load(); got != 1 {
		_ = owner.Close()
		t.Fatalf("verifier calls after retry = %d, want 1", got)
	}
	for _, absent := range []string{
		credentialEndpointMaintenanceJournalPath(path),
		credentialEndpointMaintenanceRollbackPath(path),
		filepath.Join(filepath.Dir(path), ticket.QuarantineName),
	} {
		if _, err := os.Lstat(absent); !errors.Is(err, os.ErrNotExist) {
			_ = owner.Close()
			t.Fatalf("finalised artifact %s remains: %v", filepath.Base(absent), err)
		}
	}
	for _, retained := range []string{path, credentialEndpointSidecarPath(path), credentialEndpointLockPath(path)} {
		if _, err := os.Lstat(retained); err != nil {
			_ = owner.Close()
			t.Fatalf("live owner artifact %s missing: %v", filepath.Base(retained), err)
		}
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestActivatedLegacyCredentialEndpointRollbackRestoresExactLegacySocket(t *testing.T) {
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
	if err := transition.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := transition.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := ResumeLegacyCredentialEndpointTransition(context.Background(), path, ticket, authority)
	if err != nil {
		t.Fatal(err)
	}
	if err := resumed.Rollback(context.Background()); err != nil {
		_ = resumed.Close()
		t.Fatalf("Rollback error = %v", err)
	}
	if resumed.State() != CredentialEndpointMaintenanceRolledBack {
		_ = resumed.Close()
		t.Fatalf("state = %q, want rolled_back", resumed.State())
	}
	if err := resumed.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("legacy endpoint stat has unexpected type")
	}
	proof := LegacyCredentialEndpointIdentity{
		Device: uint64(stat.Dev), Inode: uint64(stat.Ino), UID: uint64(stat.Uid), Links: uint64(stat.Nlink),
		Type: "socket", Mode: uint32(stat.Mode) & 0o777,
	}
	if proof != ticket.Socket {
		t.Fatalf("restored legacy proof = %#v, want %#v", proof, ticket.Socket)
	}
	for _, absent := range []string{
		credentialEndpointMaintenanceRollbackPath(path),
		filepath.Join(filepath.Dir(path), ticket.QuarantineName),
		credentialEndpointSidecarPath(path),
	} {
		if _, err := os.Lstat(absent); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("rollback artifact %s remains: %v", filepath.Base(absent), err)
		}
	}
	data, err := os.ReadFile(credentialEndpointMaintenanceJournalPath(path))
	if err != nil {
		t.Fatal(err)
	}
	journal, err := decodeCredentialEndpointMaintenanceJournal(data)
	if err != nil || journal.State != CredentialEndpointMaintenanceRolledBack || journal.Owner != nil {
		t.Fatalf("parked rollback journal = %#v, %v", journal, err)
	}
}

func TestResumeActivatedRollbackCrashShapes(t *testing.T) {
	for _, journalDurable := range []bool{false, true} {
		journalDurable := journalDurable
		t.Run(fmt.Sprintf("rolled_back_journal_%t", journalDurable), func(t *testing.T) {
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
			if err := transition.Activate(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := transition.Close(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(credentialEndpointMaintenanceRollbackPath(path))
			if err != nil {
				t.Fatal(err)
			}
			record, err := decodeCredentialEndpointMaintenanceJournal(data)
			if err != nil {
				t.Fatal(err)
			}
			record.Generation++
			record.State = CredentialEndpointMaintenanceRollingBack
			recordData, err := encodeCredentialEndpointMaintenanceJournal(record)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(credentialEndpointMaintenanceRollbackPath(path), recordData, 0o600); err != nil {
				t.Fatal(err)
			}
			if journalDurable {
				quarantine := filepath.Join(filepath.Dir(path), ticket.QuarantineName)
				if err := os.Link(quarantine, path); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(quarantine); err != nil {
					t.Fatal(err)
				}
				journal := record
				journal.Generation++
				journal.State = CredentialEndpointMaintenanceRolledBack
				journal.Owner = nil
				journalData, err := encodeCredentialEndpointMaintenanceJournal(journal)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(credentialEndpointMaintenanceJournalPath(path), journalData, 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				status, err := InspectLegacyCredentialEndpointTransition(context.Background(), path)
				if err != nil {
					t.Fatalf("Inspect rolling-back receipt error = %v", err)
				}
				if status.State != CredentialEndpointMaintenanceRollingBack {
					t.Fatalf("Inspect state = %q, want rolling_back", status.State)
				}
			}
			resumed, err := ResumeLegacyCredentialEndpointTransition(context.Background(), path, ticket, authority)
			if err != nil {
				t.Fatalf("Resume error = %v", err)
			}
			if resumed.State() != CredentialEndpointMaintenanceRolledBack {
				_ = resumed.Close()
				t.Fatalf("state = %q, want rolled_back", resumed.State())
			}
			if err := resumed.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(credentialEndpointMaintenanceRollbackPath(path)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rollback receipt remains: %v", err)
			}
		})
	}
}

func TestActivatedRollbackCleansOnlyExactRefusedBoundCandidate(t *testing.T) {
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
	if err := transition.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := transition.Close(); err != nil {
		t.Fatal(err)
	}

	endpoint, client, err := openCredentialEndpoint(path, false, nil)
	if err != nil || client != nil || endpoint == nil {
		t.Fatalf("candidate endpoint = %v, %v, %v", endpoint, client, err)
	}
	if err := endpoint.listener.Close(); err != nil {
		t.Fatal(err)
	}
	endpoint.release()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("crash residue endpoint missing: %v", err)
	}
	if _, err := os.Lstat(credentialEndpointSidecarPath(path)); err != nil {
		t.Fatalf("crash residue sidecar missing: %v", err)
	}

	resumed, err := ResumeLegacyCredentialEndpointTransition(context.Background(), path, ticket, authority)
	if err != nil {
		t.Fatal(err)
	}
	if err := resumed.Rollback(context.Background()); err != nil {
		_ = resumed.Close()
		t.Fatalf("Rollback error = %v", err)
	}
	if err := resumed.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(credentialEndpointSidecarPath(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("candidate sidecar remains: %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || uint64(stat.Ino) != ticket.Socket.Inode || uint64(stat.Dev) != ticket.Socket.Device {
		t.Fatalf("restored socket stat = %#v, want ticket inode", stat)
	}
}

func TestActivatedRollbackRejectsSidecarReplacementAfterDeadProbe(t *testing.T) {
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
	if err := transition.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := transition.Close(); err != nil {
		t.Fatal(err)
	}
	endpoint, client, err := openCredentialEndpoint(path, false, nil)
	if err != nil || client != nil || endpoint == nil {
		t.Fatalf("candidate endpoint = %v, %v, %v", endpoint, client, err)
	}
	if err := endpoint.listener.Close(); err != nil {
		t.Fatal(err)
	}
	endpoint.release()

	sidecarPath := credentialEndpointSidecarPath(path)
	sidecarData, err := os.ReadFile(sidecarPath)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(sidecarPath)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := ResumeLegacyCredentialEndpointTransition(context.Background(), path, ticket, authority)
	if err != nil {
		t.Fatal(err)
	}
	implementation, ok := resumed.implementation.(*legacyCredentialEndpointTransition)
	if !ok {
		_ = resumed.Close()
		t.Fatal("unexpected transition implementation")
	}
	var replaced sync.Once
	implementation.rollbackHook = func(phase credentialEndpointPhase) {
		if phase != credentialEndpointPhaseMaintenanceRollbackCandidateValidated {
			return
		}
		replaced.Do(func() {
			temporary := filepath.Join(filepath.Dir(path), ".rollback-replacement-sidecar")
			if err := os.WriteFile(temporary, sidecarData, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(temporary, sidecarPath); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err := resumed.Rollback(context.Background()); !errors.Is(err, ErrCredentialEndpointIdentityChanged) {
		_ = resumed.Close()
		t.Fatalf("Rollback error = %v, want identity conflict", err)
	}
	if err := resumed.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(sidecarPath)
	if err != nil {
		t.Fatalf("replacement sidecar was removed: %v", err)
	}
	beforeIdentity, beforeOK := (fsutil.OSFileSystem{}).FileIdentity(before)
	afterIdentity, afterOK := (fsutil.OSFileSystem{}).FileIdentity(after)
	if !beforeOK || !afterOK || beforeIdentity == afterIdentity {
		t.Fatal("test did not replace the sidecar inode")
	}
	afterData, err := os.ReadFile(sidecarPath)
	if err != nil || !bytes.Equal(afterData, sidecarData) {
		t.Fatalf("replacement sidecar changed: %v", err)
	}
	recordData, err := os.ReadFile(credentialEndpointMaintenanceRollbackPath(path))
	if err != nil {
		t.Fatal(err)
	}
	record, err := decodeCredentialEndpointMaintenanceJournal(recordData)
	if err != nil || record.State != CredentialEndpointMaintenanceActivated {
		t.Fatalf("rollback receipt = %#v, %v, want activated", record, err)
	}
	for _, retained := range []string{path, filepath.Join(filepath.Dir(path), ticket.QuarantineName)} {
		if _, err := os.Lstat(retained); err != nil {
			t.Fatalf("failed rollback removed %s: %v", filepath.Base(retained), err)
		}
	}
}

func TestActivatedRollbackRejectsClosingSidecarReplacementBeforeSocketCleanup(t *testing.T) {
	for _, finalExists := range []bool{true, false} {
		finalExists := finalExists
		t.Run(fmt.Sprintf("final_exists_%t", finalExists), func(t *testing.T) {
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
			if err := transition.Activate(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := transition.Close(); err != nil {
				t.Fatal(err)
			}
			endpoint, client, err := openCredentialEndpoint(path, false, nil)
			if err != nil || client != nil || endpoint == nil {
				t.Fatalf("candidate endpoint = %v, %v, %v", endpoint, client, err)
			}
			if err := endpoint.listener.Close(); err != nil {
				t.Fatal(err)
			}
			endpoint.release()

			sidecarPath := credentialEndpointSidecarPath(path)
			publishedData, err := os.ReadFile(sidecarPath)
			if err != nil {
				t.Fatal(err)
			}
			sidecar, err := decodeCredentialEndpointSidecar(publishedData, path)
			if err != nil {
				t.Fatal(err)
			}
			sidecar.State = credentialEndpointClosing
			closingData, err := json.Marshal(sidecar)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(sidecarPath, closingData, 0o600); err != nil {
				t.Fatal(err)
			}
			var finalBefore fsutil.SecureFileIdentity
			if finalExists {
				info, err := os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				var ok bool
				finalBefore, ok = (fsutil.OSFileSystem{}).FileIdentity(info)
				if !ok {
					t.Fatal("candidate socket identity unavailable")
				}
			} else if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(sidecarPath)
			if err != nil {
				t.Fatal(err)
			}
			resumed, err := ResumeLegacyCredentialEndpointTransition(context.Background(), path, ticket, authority)
			if err != nil {
				t.Fatal(err)
			}
			implementation, ok := resumed.implementation.(*legacyCredentialEndpointTransition)
			if !ok {
				_ = resumed.Close()
				t.Fatal("unexpected transition implementation")
			}
			var replaced sync.Once
			implementation.rollbackHook = func(phase credentialEndpointPhase) {
				if phase != credentialEndpointPhaseMaintenanceRollbackCandidateValidated {
					return
				}
				replaced.Do(func() {
					temporary := filepath.Join(filepath.Dir(path), ".rollback-closing-replacement-sidecar")
					if err := os.WriteFile(temporary, closingData, 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(temporary, sidecarPath); err != nil {
						t.Fatal(err)
					}
				})
			}
			if err := resumed.Rollback(context.Background()); !errors.Is(err, ErrCredentialEndpointIdentityChanged) {
				_ = resumed.Close()
				t.Fatalf("Rollback error = %v, want identity conflict", err)
			}
			if err := resumed.Close(); err != nil {
				t.Fatal(err)
			}
			after, err := os.Lstat(sidecarPath)
			if err != nil {
				t.Fatalf("replacement sidecar was removed: %v", err)
			}
			beforeIdentity, beforeOK := (fsutil.OSFileSystem{}).FileIdentity(before)
			afterIdentity, afterOK := (fsutil.OSFileSystem{}).FileIdentity(after)
			if !beforeOK || !afterOK || beforeIdentity == afterIdentity {
				t.Fatal("test did not replace the closing sidecar inode")
			}
			if finalExists {
				info, err := os.Lstat(path)
				if err != nil {
					t.Fatalf("failed rollback removed candidate socket: %v", err)
				}
				identity, ok := (fsutil.OSFileSystem{}).FileIdentity(info)
				if !ok || identity != finalBefore {
					t.Fatal("failed rollback replaced candidate socket")
				}
			} else if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed rollback recreated candidate socket: %v", err)
			}
			for _, retained := range []string{
				credentialEndpointMaintenanceRollbackPath(path),
				filepath.Join(filepath.Dir(path), ticket.QuarantineName),
			} {
				if _, err := os.Lstat(retained); err != nil {
					t.Fatalf("failed rollback removed %s: %v", filepath.Base(retained), err)
				}
			}
		})
	}
}

func TestActivatedRollbackCleansExactClosingCandidate(t *testing.T) {
	for _, finalExists := range []bool{true, false} {
		finalExists := finalExists
		t.Run(fmt.Sprintf("final_exists_%t", finalExists), func(t *testing.T) {
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
			if err := transition.Activate(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := transition.Close(); err != nil {
				t.Fatal(err)
			}
			endpoint, client, err := openCredentialEndpoint(path, false, nil)
			if err != nil || client != nil || endpoint == nil {
				t.Fatalf("candidate endpoint = %v, %v, %v", endpoint, client, err)
			}
			if err := endpoint.listener.Close(); err != nil {
				t.Fatal(err)
			}
			endpoint.release()

			sidecarPath := credentialEndpointSidecarPath(path)
			publishedData, err := os.ReadFile(sidecarPath)
			if err != nil {
				t.Fatal(err)
			}
			sidecar, err := decodeCredentialEndpointSidecar(publishedData, path)
			if err != nil {
				t.Fatal(err)
			}
			sidecar.State = credentialEndpointClosing
			closingData, err := json.Marshal(sidecar)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(sidecarPath, closingData, 0o600); err != nil {
				t.Fatal(err)
			}
			if !finalExists {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}

			resumed, err := ResumeLegacyCredentialEndpointTransition(context.Background(), path, ticket, authority)
			if err != nil {
				t.Fatal(err)
			}
			if err := resumed.Rollback(context.Background()); err != nil {
				_ = resumed.Close()
				t.Fatalf("Rollback error = %v", err)
			}
			if err := resumed.Close(); err != nil {
				t.Fatal(err)
			}

			info, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				t.Fatal("restored legacy endpoint stat has unexpected type")
			}
			proof := LegacyCredentialEndpointIdentity{
				Device: uint64(stat.Dev), Inode: uint64(stat.Ino), UID: uint64(stat.Uid), Links: uint64(stat.Nlink),
				Type: "socket", Mode: uint32(stat.Mode) & 0o777,
			}
			if proof != ticket.Socket {
				t.Fatalf("restored legacy proof = %#v, want %#v", proof, ticket.Socket)
			}
			for _, absent := range []string{
				credentialEndpointMaintenanceRollbackPath(path),
				filepath.Join(filepath.Dir(path), ticket.QuarantineName),
				credentialEndpointSidecarPath(path),
			} {
				if _, err := os.Lstat(absent); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("rollback artifact %s remains: %v", filepath.Base(absent), err)
				}
			}
			data, err := os.ReadFile(credentialEndpointMaintenanceJournalPath(path))
			if err != nil {
				t.Fatal(err)
			}
			journal, err := decodeCredentialEndpointMaintenanceJournal(data)
			if err != nil || journal.State != CredentialEndpointMaintenanceRolledBack || journal.Owner != nil {
				t.Fatalf("parked rollback journal = %#v, %v", journal, err)
			}
		})
	}
}

func TestOfflineFinaliseRollsForwardOnlyDurableFinalisingReceipt(t *testing.T) {
	t.Parallel()
	path := createRefusedLegacyCredentialSocket(t)
	snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	transition, err := PrepareLegacyCredentialEndpointTransition(
		context.Background(), path, snapshot,
		DrainAuthorityFunc(func(context.Context, string) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	ticket := transition.Ticket()
	if err := transition.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := transition.Close(); err != nil {
		t.Fatal(err)
	}
	endpoint, client, err := openCredentialEndpoint(path, false, nil)
	if err != nil || client != nil || endpoint == nil {
		t.Fatalf("candidate endpoint = %v, %v, %v", endpoint, client, err)
	}
	data, err := os.ReadFile(credentialEndpointMaintenanceRollbackPath(path))
	if err != nil {
		t.Fatal(err)
	}
	record, err := decodeCredentialEndpointMaintenanceJournal(data)
	if err != nil || record.Owner == nil {
		t.Fatalf("bound receipt = %#v, %v", record, err)
	}
	record.Generation++
	record.State = CredentialEndpointMaintenanceFinalising
	data, err = encodeCredentialEndpointMaintenanceJournal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentialEndpointMaintenanceRollbackPath(path), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := endpoint.listener.Close(); err != nil {
		t.Fatal(err)
	}
	endpoint.release()

	if err := FinaliseLegacyCredentialEndpointTransition(context.Background(), path, ticket); err != nil {
		t.Fatalf("offline Finalise error = %v", err)
	}
	for _, absent := range []string{
		credentialEndpointMaintenanceRollbackPath(path),
		filepath.Join(filepath.Dir(path), ticket.QuarantineName),
	} {
		if _, err := os.Lstat(absent); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("finalise artifact %s remains: %v", filepath.Base(absent), err)
		}
	}
	for _, preserved := range []string{path, credentialEndpointSidecarPath(path), credentialEndpointLockPath(path)} {
		if _, err := os.Lstat(preserved); err != nil {
			t.Fatalf("offline finalise changed candidate residue %s: %v", filepath.Base(preserved), err)
		}
	}
}

func TestOfflineFinaliseResumesAfterQuarantineRemoval(t *testing.T) {
	t.Parallel()
	path := createRefusedLegacyCredentialSocket(t)
	snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	transition, err := PrepareLegacyCredentialEndpointTransition(
		context.Background(), path, snapshot,
		DrainAuthorityFunc(func(context.Context, string) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	ticket := transition.Ticket()
	if err := transition.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := transition.Close(); err != nil {
		t.Fatal(err)
	}
	endpoint, client, err := openCredentialEndpoint(path, false, nil)
	if err != nil || client != nil || endpoint == nil {
		t.Fatalf("candidate endpoint = %v, %v, %v", endpoint, client, err)
	}
	receiptPath := credentialEndpointMaintenanceRollbackPath(path)
	data, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	record, err := decodeCredentialEndpointMaintenanceJournal(data)
	if err != nil || record.Owner == nil {
		t.Fatalf("bound receipt = %#v, %v", record, err)
	}
	record.Generation++
	record.State = CredentialEndpointMaintenanceFinalising
	data, err = encodeCredentialEndpointMaintenanceJournal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(receiptPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := endpoint.listener.Close(); err != nil {
		t.Fatal(err)
	}
	endpoint.release()
	quarantinePath := filepath.Join(filepath.Dir(path), ticket.QuarantineName)
	if err := os.Remove(quarantinePath); err != nil {
		t.Fatal(err)
	}

	if err := FinaliseLegacyCredentialEndpointTransition(context.Background(), path, ticket); err != nil {
		t.Fatalf("offline Finalise error = %v", err)
	}
	for _, absent := range []string{receiptPath, quarantinePath} {
		if _, err := os.Lstat(absent); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("finalise artifact %s remains: %v", filepath.Base(absent), err)
		}
	}
	for _, preserved := range []string{path, credentialEndpointSidecarPath(path), credentialEndpointLockPath(path)} {
		if _, err := os.Lstat(preserved); err != nil {
			t.Fatalf("offline finalise changed candidate residue %s: %v", filepath.Base(preserved), err)
		}
	}
}

func TestFinaliseVerifierFailureIsZeroWriteAndPrivate(t *testing.T) {
	for _, test := range []struct {
		name     string
		verifier LegacyMaintenanceFinaliseVerifier
		wantErr  error
	}{
		{name: "missing", wantErr: ErrCredentialEndpointMaintenanceVerifierRequired},
		{
			name: "runtime gate rejected",
			verifier: testLegacyMaintenanceFinaliseVerifier(func(context.Context, LegacyMaintenanceFinaliseVerification) error {
				return errors.New("SECRET_RUNTIME_CANARY")
			}),
			wantErr: ErrCredentialEndpointMaintenanceVerification,
		},
		{
			name: "nil lease",
			verifier: LegacyMaintenanceFinaliseVerifierFunc(func(context.Context, LegacyMaintenanceFinaliseVerification) (LegacyMaintenanceFinaliseLease, error) {
				return nil, nil
			}),
			wantErr: ErrCredentialEndpointMaintenanceVerifierRequired,
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := createRefusedLegacyCredentialSocket(t)
			snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			transition, err := PrepareLegacyCredentialEndpointTransition(
				context.Background(), path, snapshot,
				DrainAuthorityFunc(func(context.Context, string) error { return nil }),
			)
			if err != nil {
				t.Fatal(err)
			}
			ticket := transition.Ticket()
			if err := transition.Activate(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := transition.Close(); err != nil {
				t.Fatal(err)
			}
			coordinator, _ := testCoordinator(t)
			owner, err := openCredentialControlPreparedWithLegacyMaintenanceVerifier(
				context.Background(), path, coordinator, false, nil, nil, nil, test.verifier,
			)
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(credentialEndpointMaintenanceRollbackPath(path))
			if err != nil {
				_ = owner.Close()
				t.Fatal(err)
			}
			record, err := decodeCredentialEndpointMaintenanceJournal(before)
			if err != nil || record.Owner == nil {
				_ = owner.Close()
				t.Fatalf("bound record = %#v, %v", record, err)
			}
			err = FinaliseLegacyCredentialEndpointTransition(context.Background(), path, ticket)
			if !errors.Is(err, test.wantErr) {
				_ = owner.Close()
				t.Fatalf("Finalise error = %v, want %v", err, test.wantErr)
			}
			for _, secret := range []string{"SECRET_RUNTIME_CANARY", ticket.ID, record.Owner.Generation} {
				if strings.Contains(err.Error(), secret) {
					_ = owner.Close()
					t.Fatalf("Finalise error exposed private value %q: %v", secret, err)
				}
			}
			after, err := os.ReadFile(credentialEndpointMaintenanceRollbackPath(path))
			if err != nil {
				_ = owner.Close()
				t.Fatal(err)
			}
			if !bytes.Equal(after, before) {
				_ = owner.Close()
				t.Fatal("failed verifier rewrote rollback receipt")
			}
			if _, err := os.Lstat(filepath.Join(filepath.Dir(path), ticket.QuarantineName)); err != nil {
				_ = owner.Close()
				t.Fatalf("failed verifier removed quarantine: %v", err)
			}
			if err := owner.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFinaliseRPCWrongTicketAndGenerationAreZeroWrite(t *testing.T) {
	t.Parallel()
	path := createRefusedLegacyCredentialSocket(t)
	snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	transition, err := PrepareLegacyCredentialEndpointTransition(
		context.Background(), path, snapshot,
		DrainAuthorityFunc(func(context.Context, string) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	ticket := transition.Ticket()
	if err := transition.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := transition.Close(); err != nil {
		t.Fatal(err)
	}
	var verifierCalls atomic.Int32
	coordinator, _ := testCoordinator(t)
	owner, err := openCredentialControlPreparedWithLegacyMaintenanceVerifier(
		context.Background(), path, coordinator, false, nil, nil, nil,
		testLegacyMaintenanceFinaliseVerifier(func(context.Context, LegacyMaintenanceFinaliseVerification) error {
			verifierCalls.Add(1)
			return nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	receiptPath := credentialEndpointMaintenanceRollbackPath(path)
	before, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	record, err := decodeCredentialEndpointMaintenanceJournal(before)
	if err != nil || record.Owner == nil {
		t.Fatalf("bound receipt = %#v, %v", record, err)
	}
	client, err := dialCredentialOwner(path, credentialEndpointDialTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	wrongTicket := ticket
	wrongTicket.ID = strings.Repeat("c", 32)
	if wrongTicket.ID == ticket.ID {
		wrongTicket.ID = strings.Repeat("d", 32)
	}
	wrongTicket.QuarantineName = "." + filepath.Base(path) + ".legacy-" + wrongTicket.ID + ".quarantine"
	for _, args := range []LegacyCredentialEndpointFinaliseRPCArgs{
		{Ticket: ticket, OwnerGeneration: strings.Repeat("d", 32)},
		{Ticket: wrongTicket, OwnerGeneration: record.Owner.Generation},
	} {
		var reply LegacyCredentialEndpointFinaliseRPCReply
		if err := client.Call("CredentialEndpoint.FinaliseMaintenance", args, &reply); err == nil {
			t.Fatal("unauthorised finalise RPC succeeded")
		}
		after, err := os.ReadFile(receiptPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(after, before) {
			t.Fatal("unauthorised finalise RPC rewrote receipt")
		}
		if _, err := os.Lstat(filepath.Join(filepath.Dir(path), ticket.QuarantineName)); err != nil {
			t.Fatalf("unauthorised finalise RPC removed quarantine: %v", err)
		}
	}
	if got := verifierCalls.Load(); got != 0 {
		t.Fatalf("verifier calls = %d, want 0", got)
	}
}

func TestFinaliseOwnerOperationLinearisesWithClose(t *testing.T) {
	t.Parallel()
	path := createRefusedLegacyCredentialSocket(t)
	snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	transition, err := PrepareLegacyCredentialEndpointTransition(
		context.Background(), path, snapshot,
		DrainAuthorityFunc(func(context.Context, string) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	ticket := transition.Ticket()
	if err := transition.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := transition.Close(); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	verifier := testLegacyMaintenanceFinaliseVerifier(func(context.Context, LegacyMaintenanceFinaliseVerification) error {
		close(entered)
		<-release
		return nil
	})
	coordinator, _ := testCoordinator(t)
	owner, err := openCredentialControlPreparedWithLegacyMaintenanceVerifier(
		context.Background(), path, coordinator, false, nil, nil, nil, verifier,
	)
	if err != nil {
		t.Fatal(err)
	}
	finaliseDone := make(chan error, 1)
	go func() { finaliseDone <- FinaliseLegacyCredentialEndpointTransition(context.Background(), path, ticket) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		unblock()
		_ = owner.Close()
		t.Fatal("verifier was not entered")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- owner.Close() }()
	select {
	case err := <-closeDone:
		unblock()
		t.Fatalf("Close returned before finalise operation released: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	unblock()
	if err := <-finaliseDone; err != nil {
		t.Fatalf("Finalise error = %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close error = %v", err)
	}
	for _, absent := range []string{
		credentialEndpointMaintenanceRollbackPath(path),
		filepath.Join(filepath.Dir(path), ticket.QuarantineName),
		path,
		credentialEndpointSidecarPath(path),
	} {
		if _, err := os.Lstat(absent); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("post-close artifact %s remains: %v", filepath.Base(absent), err)
		}
	}
}

func TestFinaliseServingLeaseSpansDurableFinalisingReceipt(t *testing.T) {
	t.Parallel()
	path := createRefusedLegacyCredentialSocket(t)
	snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	transition, err := PrepareLegacyCredentialEndpointTransition(
		context.Background(), path, snapshot,
		DrainAuthorityFunc(func(context.Context, string) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	ticket := transition.Ticket()
	if err := transition.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := transition.Close(); err != nil {
		t.Fatal(err)
	}

	releaseStarted := make(chan struct{})
	allowRelease := make(chan struct{})
	var allowReleaseOnce sync.Once
	allow := func() { allowReleaseOnce.Do(func() { close(allowRelease) }) }
	lease := &blockingLegacyMaintenanceFinaliseLease{
		started: releaseStarted,
		allow:   allowRelease,
	}
	coordinator, _ := testCoordinator(t)
	owner, err := openCredentialControlPreparedWithLegacyMaintenanceVerifier(
		context.Background(), path, coordinator, false, nil, nil, nil,
		legacyMaintenanceFinaliseLeaseVerifier{lease: lease},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	t.Cleanup(allow)

	finaliseDone := make(chan error, 1)
	go func() {
		finaliseDone <- FinaliseLegacyCredentialEndpointTransition(context.Background(), path, ticket)
	}()
	select {
	case <-releaseStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("finalise lease was not released")
	}
	select {
	case err := <-finaliseDone:
		t.Fatalf("Finalise returned before the serving lease released: %v", err)
	default:
	}
	receipt, err := os.ReadFile(credentialEndpointMaintenanceRollbackPath(path))
	if err != nil {
		t.Fatal(err)
	}
	record, err := decodeCredentialEndpointMaintenanceJournal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != CredentialEndpointMaintenanceFinalising {
		t.Fatalf("receipt state while lease held = %q, want finalising", record.State)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(path), ticket.QuarantineName)); err != nil {
		t.Fatalf("quarantine removed before serving lease released: %v", err)
	}

	allow()
	if err := <-finaliseDone; err != nil {
		t.Fatalf("Finalise error = %v", err)
	}
	lease.Release()
	if got := lease.calls.Load(); got != 1 {
		t.Fatalf("lease release calls = %d, want idempotent 1", got)
	}
}

func TestFinaliseAcquireFailureReleasesLeaseAndPreservesReceipt(t *testing.T) {
	t.Parallel()
	path := createRefusedLegacyCredentialSocket(t)
	snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	transition, err := PrepareLegacyCredentialEndpointTransition(
		context.Background(), path, snapshot,
		DrainAuthorityFunc(func(context.Context, string) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	ticket := transition.Ticket()
	if err := transition.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := transition.Close(); err != nil {
		t.Fatal(err)
	}

	allowRelease := make(chan struct{})
	close(allowRelease)
	lease := &blockingLegacyMaintenanceFinaliseLease{
		started: make(chan struct{}),
		allow:   allowRelease,
	}
	coordinator, _ := testCoordinator(t)
	owner, err := openCredentialControlPreparedWithLegacyMaintenanceVerifier(
		context.Background(), path, coordinator, false, nil, nil, nil,
		legacyMaintenanceFinaliseLeaseVerifier{lease: lease, err: errors.New("PRIVATE_ACQUIRE_FAILURE")},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	receiptPath := credentialEndpointMaintenanceRollbackPath(path)
	before, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}

	err = FinaliseLegacyCredentialEndpointTransition(context.Background(), path, ticket)
	if !errors.Is(err, ErrCredentialEndpointMaintenanceVerification) {
		t.Fatalf("Finalise error = %v, want verification failure", err)
	}
	select {
	case <-lease.started:
	case <-time.After(2 * time.Second):
		t.Fatal("failed acquisition did not release its lease")
	}
	if got := lease.calls.Load(); got != 1 {
		t.Fatalf("lease release calls = %d, want 1", got)
	}
	after, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("failed acquisition rewrote rollback receipt")
	}
}

func TestFinaliseServingLeaseOrdersDurableReceiptStages(t *testing.T) {
	t.Parallel()
	path, ticket, endpoint := openActivatedMaintenanceEndpointForFinaliseTest(t)
	sequence := &finaliseStageSequence{}
	trackedDirectory := &finaliseStageDirectory{
		SecureDirectory: endpoint.secureDirectory,
		recordName:      filepath.Base(credentialEndpointMaintenanceRollbackPath(path)),
		sequence:        sequence,
	}
	endpoint.secureDirectory = trackedDirectory
	generation := endpoint.maintenanceGate.record.Owner.Generation
	lease := &finaliseStageLease{sequence: sequence}
	verifier := legacyMaintenanceFinaliseLeaseVerifier{
		lease: lease,
		verify: func(context.Context, LegacyMaintenanceFinaliseVerification) error {
			sequence.add("acquire")
			trackedDirectory.arm()
			return nil
		},
	}

	if err := endpoint.finaliseActivatedMaintenance(context.Background(), ticket, generation, verifier); err != nil {
		t.Fatal(err)
	}
	stages := sequence.snapshot()
	for _, required := range [][]string{
		{"temp-sync", "acquire"},
		{"acquire", "rename"},
		{"rename", "sync"},
		{"sync", "read-after-sync"},
		{"read-after-sync", "release"},
	} {
		if !finaliseStagesOrdered(stages, required[0], required[1]) {
			t.Fatalf("finalise stages = %v, want %q before %q", stages, required[0], required[1])
		}
	}
	if attempts, effects := lease.attempts.Load(), lease.effects.Load(); attempts != 1 || effects != 1 {
		t.Fatalf("lease release attempts/effects = %d/%d, want 1/1", attempts, effects)
	}
}

func TestFinaliseServingLeaseSyncFailureBlocksLiveRetryAndOfflineRecovers(t *testing.T) {
	t.Parallel()
	path, ticket, endpoint := openActivatedMaintenanceEndpointForFinaliseTest(t)
	sequence := &finaliseStageSequence{}
	trackedDirectory := &finaliseStageDirectory{
		SecureDirectory: endpoint.secureDirectory,
		recordName:      filepath.Base(credentialEndpointMaintenanceRollbackPath(path)),
		sequence:        sequence,
		failSync:        true,
	}
	endpoint.secureDirectory = trackedDirectory
	generation := endpoint.maintenanceGate.record.Owner.Generation
	lease := &finaliseStageLease{sequence: sequence}
	var acquireCalls atomic.Int32
	verifier := legacyMaintenanceFinaliseLeaseVerifier{
		lease: lease,
		verify: func(context.Context, LegacyMaintenanceFinaliseVerification) error {
			acquireCalls.Add(1)
			sequence.add("acquire")
			trackedDirectory.arm()
			return nil
		},
	}

	err := endpoint.finaliseActivatedMaintenance(context.Background(), ticket, generation, verifier)
	if err == nil {
		t.Fatal("Finalise succeeded after an indeterminate directory sync")
	}
	stages := sequence.snapshot()
	if !finaliseStagesOrdered(stages, "acquire", "rename") ||
		!finaliseStagesOrdered(stages, "rename", "sync-error") ||
		!finaliseStagesOrdered(stages, "sync-error", "release") {
		t.Fatalf("finalise failure stages = %v", stages)
	}
	if attempts, effects := lease.attempts.Load(), lease.effects.Load(); attempts != 1 || effects != 1 {
		t.Fatalf("lease release attempts/effects = %d/%d, want 1/1", attempts, effects)
	}
	quarantinePath := filepath.Join(filepath.Dir(path), ticket.QuarantineName)
	receiptPath := credentialEndpointMaintenanceRollbackPath(path)
	quarantineBefore, err := os.Lstat(quarantinePath)
	if err != nil {
		t.Fatalf("indeterminate finalise removed quarantine: %v", err)
	}
	receiptBefore, err := os.Lstat(receiptPath)
	if err != nil {
		t.Fatalf("indeterminate finalise removed receipt: %v", err)
	}
	receiptDataBefore, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}

	err = endpoint.finaliseActivatedMaintenance(context.Background(), ticket, generation, verifier)
	if !errors.Is(err, ErrCredentialEndpointMaintenanceConflict) {
		t.Fatalf("live retry error = %v, want maintenance conflict", err)
	}
	if got := acquireCalls.Load(); got != 1 {
		t.Fatalf("live retry acquisition calls = %d, want 1 total", got)
	}
	quarantineAfter, err := os.Lstat(quarantinePath)
	if err != nil || !os.SameFile(quarantineBefore, quarantineAfter) {
		t.Fatalf("live retry changed quarantine identity: before=%v after=%v err=%v", quarantineBefore, quarantineAfter, err)
	}
	receiptAfter, err := os.Lstat(receiptPath)
	if err != nil || !os.SameFile(receiptBefore, receiptAfter) {
		t.Fatalf("live retry changed receipt identity: before=%v after=%v err=%v", receiptBefore, receiptAfter, err)
	}
	receiptDataAfter, err := os.ReadFile(receiptPath)
	if err != nil || !bytes.Equal(receiptDataBefore, receiptDataAfter) {
		t.Fatalf("live retry changed receipt bytes: err=%v", err)
	}

	// A clean owner stop establishes a fresh offline recovery boundary. Restore the
	// real directory before closing so the injected sync failure remains confined
	// to the original activated-to-finalising attempt.
	endpoint.secureDirectory = trackedDirectory.SecureDirectory
	if err := endpoint.Close(); err != nil {
		t.Fatalf("clean owner stop: %v", err)
	}
	if err := FinaliseLegacyCredentialEndpointTransition(context.Background(), path, ticket); err != nil {
		t.Fatalf("offline finalise after clean stop: %v", err)
	}
	for _, removed := range []string{quarantinePath, receiptPath} {
		if _, err := os.Lstat(removed); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("offline finalise retained %s: %v", filepath.Base(removed), err)
		}
	}
}

func TestFinaliseServingLeaseReleasesOnPostAcquireRenameFailure(t *testing.T) {
	t.Parallel()
	path, ticket, endpoint := openActivatedMaintenanceEndpointForFinaliseTest(t)
	sequence := &finaliseStageSequence{}
	trackedDirectory := &finaliseStageDirectory{
		SecureDirectory: endpoint.secureDirectory,
		recordName:      filepath.Base(credentialEndpointMaintenanceRollbackPath(path)),
		sequence:        sequence,
		failRename:      true,
	}
	endpoint.secureDirectory = trackedDirectory
	generation := endpoint.maintenanceGate.record.Owner.Generation
	lease := &finaliseStageLease{sequence: sequence}
	verifier := legacyMaintenanceFinaliseLeaseVerifier{
		lease: lease,
		verify: func(context.Context, LegacyMaintenanceFinaliseVerification) error {
			sequence.add("acquire")
			trackedDirectory.arm()
			return nil
		},
	}

	err := endpoint.finaliseActivatedMaintenance(context.Background(), ticket, generation, verifier)
	if err == nil {
		t.Fatal("Finalise succeeded after an injected rename failure")
	}
	stages := sequence.snapshot()
	if !finaliseStagesOrdered(stages, "acquire", "rename-error") ||
		!finaliseStagesOrdered(stages, "rename-error", "release") {
		t.Fatalf("rename failure stages = %v", stages)
	}
	if attempts, effects := lease.attempts.Load(), lease.effects.Load(); attempts != 1 || effects != 1 {
		t.Fatalf("lease release attempts/effects = %d/%d, want 1/1", attempts, effects)
	}
	receiptData, err := os.ReadFile(credentialEndpointMaintenanceRollbackPath(path))
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := decodeCredentialEndpointMaintenanceJournal(receiptData)
	if err != nil || receipt.State != CredentialEndpointMaintenanceActivated {
		t.Fatalf("receipt after rename failure = %#v, %v", receipt, err)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(path), ticket.QuarantineName)); err != nil {
		t.Fatalf("rename failure removed quarantine: %v", err)
	}
}

func TestFinaliseServingLeasePostSyncReadFailureRequiresOfflineRecovery(t *testing.T) {
	t.Parallel()
	path, ticket, endpoint := openActivatedMaintenanceEndpointForFinaliseTest(t)
	sequence := &finaliseStageSequence{}
	trackedDirectory := &finaliseStageDirectory{
		SecureDirectory:   endpoint.secureDirectory,
		recordName:        filepath.Base(credentialEndpointMaintenanceRollbackPath(path)),
		sequence:          sequence,
		failReadAfterSync: true,
	}
	endpoint.secureDirectory = trackedDirectory
	generation := endpoint.maintenanceGate.record.Owner.Generation
	lease := &finaliseStageLease{sequence: sequence}
	var acquireCalls atomic.Int32
	verifier := legacyMaintenanceFinaliseLeaseVerifier{
		lease: lease,
		verify: func(context.Context, LegacyMaintenanceFinaliseVerification) error {
			acquireCalls.Add(1)
			sequence.add("acquire")
			trackedDirectory.arm()
			return nil
		},
	}

	err := endpoint.finaliseActivatedMaintenance(context.Background(), ticket, generation, verifier)
	if err == nil {
		t.Fatal("Finalise succeeded after an injected post-sync receipt read failure")
	}
	stages := sequence.snapshot()
	for _, required := range [][]string{{"acquire", "rename"}, {"rename", "sync"}, {"sync", "read-after-sync-error"}, {"read-after-sync-error", "release"}} {
		if !finaliseStagesOrdered(stages, required[0], required[1]) {
			t.Fatalf("post-sync read failure stages = %v, want %q before %q", stages, required[0], required[1])
		}
	}
	if attempts, effects := lease.attempts.Load(), lease.effects.Load(); attempts != 1 || effects != 1 {
		t.Fatalf("lease release attempts/effects = %d/%d, want 1/1", attempts, effects)
	}
	receiptData, err := os.ReadFile(credentialEndpointMaintenanceRollbackPath(path))
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := decodeCredentialEndpointMaintenanceJournal(receiptData)
	if err != nil || receipt.State != CredentialEndpointMaintenanceFinalising {
		t.Fatalf("durable receipt after read failure = %#v, %v", receipt, err)
	}
	if err := endpoint.finaliseActivatedMaintenance(context.Background(), ticket, generation, verifier); !errors.Is(err, ErrCredentialEndpointMaintenanceConflict) {
		t.Fatalf("live retry error = %v, want maintenance conflict", err)
	}
	if got := acquireCalls.Load(); got != 1 {
		t.Fatalf("live retry acquisition calls = %d, want 1 total", got)
	}

	endpoint.secureDirectory = trackedDirectory.SecureDirectory
	if err := endpoint.Close(); err != nil {
		t.Fatalf("clean owner stop: %v", err)
	}
	if err := FinaliseLegacyCredentialEndpointTransition(context.Background(), path, ticket); err != nil {
		t.Fatalf("offline finalise after durable read failure: %v", err)
	}
}

func TestFinaliseExactFinalisingGateResumesWithoutReacquiring(t *testing.T) {
	t.Parallel()
	path, ticket, endpoint := openActivatedMaintenanceEndpointForFinaliseTest(t)
	sequence := &finaliseStageSequence{}
	trackedDirectory := &finaliseStageDirectory{
		SecureDirectory:     endpoint.secureDirectory,
		recordName:          filepath.Base(credentialEndpointMaintenanceRollbackPath(path)),
		sequence:            sequence,
		failPostReceiptSync: true,
	}
	endpoint.secureDirectory = trackedDirectory
	generation := endpoint.maintenanceGate.record.Owner.Generation
	lease := &finaliseStageLease{sequence: sequence}
	var acquireCalls atomic.Int32
	verifier := legacyMaintenanceFinaliseLeaseVerifier{
		lease: lease,
		verify: func(context.Context, LegacyMaintenanceFinaliseVerification) error {
			acquireCalls.Add(1)
			sequence.add("acquire")
			trackedDirectory.arm()
			return nil
		},
	}

	err := endpoint.finaliseActivatedMaintenance(context.Background(), ticket, generation, verifier)
	if err == nil {
		t.Fatal("Finalise succeeded after an injected quarantine directory sync failure")
	}
	if !finaliseStagesOrdered(sequence.snapshot(), "release", "post-receipt-sync-error") {
		t.Fatalf("finalise stages = %v, want release before cleanup sync failure", sequence.snapshot())
	}
	if got := acquireCalls.Load(); got != 1 {
		t.Fatalf("initial acquisition calls = %d, want 1", got)
	}
	receiptData, err := os.ReadFile(credentialEndpointMaintenanceRollbackPath(path))
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := decodeCredentialEndpointMaintenanceJournal(receiptData)
	if err != nil || receipt.State != CredentialEndpointMaintenanceFinalising {
		t.Fatalf("receipt after cleanup sync failure = %#v, %v", receipt, err)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(path), ticket.QuarantineName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("quarantine after unlink sync failure = %v, want absent", err)
	}

	if err := endpoint.finaliseActivatedMaintenance(context.Background(), ticket, generation, verifier); err != nil {
		t.Fatalf("exact Finalising retry error = %v", err)
	}
	if got := acquireCalls.Load(); got != 1 {
		t.Fatalf("Finalising retry acquisition calls = %d, want 1 total", got)
	}
	if _, err := os.Lstat(credentialEndpointMaintenanceRollbackPath(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Finalising retry retained receipt: %v", err)
	}
}

func TestFinaliseRejectsSameByteReceiptReplacementBeforeQuarantineRemoval(t *testing.T) {
	t.Parallel()
	path, ticket, endpoint := openActivatedMaintenanceEndpointForFinaliseTest(t)
	finalising := endpoint.maintenanceGate.record
	finalising.Generation++
	finalising.State = CredentialEndpointMaintenanceFinalising
	data, err := encodeCredentialEndpointMaintenanceJournal(finalising)
	if err != nil {
		t.Fatal(err)
	}
	directory := &replaceFinalisingReceiptOnReadDirectory{
		SecureDirectory: endpoint.secureDirectory,
		recordName:      filepath.Base(credentialEndpointMaintenanceRollbackPath(path)),
		recordData:      data,
	}
	endpoint.secureDirectory = directory
	generation := endpoint.maintenanceGate.record.Owner.Generation
	lease := &finaliseCallbackLease{release: directory.arm}

	err = endpoint.finaliseActivatedMaintenance(
		context.Background(), ticket, generation,
		legacyMaintenanceFinaliseLeaseVerifier{lease: lease},
	)
	if !errors.Is(err, ErrCredentialEndpointIdentityChanged) {
		t.Fatalf("Finalise error = %v, want identity changed", err)
	}
	if got := directory.replacements.Load(); got != 1 {
		t.Fatalf("same-byte receipt replacements = %d, want 1", got)
	}
	if got := lease.calls.Load(); got != 1 {
		t.Fatalf("lease release calls = %d, want 1", got)
	}
	if _, statErr := os.Lstat(filepath.Join(filepath.Dir(path), ticket.QuarantineName)); statErr != nil {
		t.Fatalf("same-byte receipt replacement removed quarantine: %v", statErr)
	}
	if _, statErr := os.Lstat(credentialEndpointMaintenanceRollbackPath(path)); statErr != nil {
		t.Fatalf("same-byte receipt replacement removed receipt: %v", statErr)
	}
}

func TestOfflineFinaliseRejectsSameByteReceiptReplacementBeforeQuarantineRemoval(t *testing.T) {
	t.Parallel()
	path, ticket, endpoint := openActivatedMaintenanceEndpointForFinaliseTest(t)
	trackedDirectory := &finaliseStageDirectory{
		SecureDirectory:   endpoint.secureDirectory,
		recordName:        filepath.Base(credentialEndpointMaintenanceRollbackPath(path)),
		sequence:          &finaliseStageSequence{},
		failReadAfterSync: true,
	}
	endpoint.secureDirectory = trackedDirectory
	generation := endpoint.maintenanceGate.record.Owner.Generation
	if err := endpoint.finaliseActivatedMaintenance(
		context.Background(), ticket, generation,
		legacyMaintenanceFinaliseLeaseVerifier{
			lease: noopLegacyMaintenanceFinaliseLease{},
			verify: func(context.Context, LegacyMaintenanceFinaliseVerification) error {
				trackedDirectory.arm()
				return nil
			},
		},
	); err == nil {
		t.Fatal("fixture finalise unexpectedly completed")
	}
	endpoint.secureDirectory = trackedDirectory.SecureDirectory
	if err := endpoint.Close(); err != nil {
		t.Fatal(err)
	}
	receiptPath := credentialEndpointMaintenanceRollbackPath(path)
	recordData, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	namespace, err := openLegacyCredentialMaintenanceNamespace(path, ticket.Directory)
	if err != nil {
		t.Fatal(err)
	}
	defer namespace.Close()
	directory := &replaceFinalisingReceiptOnReadDirectory{
		SecureDirectory: namespace.directory,
		recordName:      filepath.Base(receiptPath),
		recordData:      recordData,
		armOnProbeAt:    3,
	}
	namespace.directory = directory

	err = finaliseLegacyCredentialEndpointTransitionOfflineInNamespace(context.Background(), path, ticket, namespace)
	if !errors.Is(err, ErrCredentialEndpointIdentityChanged) {
		t.Fatalf("offline Finalise error = %v, want identity changed (replacements=%d)", err, directory.replacements.Load())
	}
	if got := directory.replacements.Load(); got != 1 {
		t.Fatalf("offline same-byte receipt replacements = %d, want 1", got)
	}
	for _, retained := range []string{receiptPath, filepath.Join(filepath.Dir(path), ticket.QuarantineName)} {
		if _, err := os.Lstat(retained); err != nil {
			t.Fatalf("offline replacement removed %s: %v", filepath.Base(retained), err)
		}
	}
}

func TestFinaliseRejectsSameByteReceiptReplacementBeforeReceiptRemoval(t *testing.T) {
	t.Parallel()
	path, ticket, endpoint := openActivatedMaintenanceEndpointForFinaliseTest(t)
	finalising := endpoint.maintenanceGate.record
	finalising.Generation++
	finalising.State = CredentialEndpointMaintenanceFinalising
	recordData, err := encodeCredentialEndpointMaintenanceJournal(finalising)
	if err != nil {
		t.Fatal(err)
	}
	replacementDirectory := &replaceFinalisingReceiptOnReadDirectory{
		SecureDirectory: endpoint.secureDirectory,
		recordName:      filepath.Base(credentialEndpointMaintenanceRollbackPath(path)),
		recordData:      recordData,
	}
	sequence := &finaliseStageSequence{}
	trackedDirectory := &finaliseStageDirectory{
		SecureDirectory: replacementDirectory,
		recordName:      replacementDirectory.recordName,
		sequence:        sequence,
		afterPostReceiptSync: func() {
			replacementDirectory.arm()
		},
	}
	endpoint.secureDirectory = trackedDirectory
	generation := endpoint.maintenanceGate.record.Owner.Generation
	lease := &finaliseStageLease{sequence: sequence}

	err = endpoint.finaliseActivatedMaintenance(
		context.Background(), ticket, generation,
		legacyMaintenanceFinaliseLeaseVerifier{
			lease: lease,
			verify: func(context.Context, LegacyMaintenanceFinaliseVerification) error {
				sequence.add("acquire")
				trackedDirectory.arm()
				return nil
			},
		},
	)
	if !errors.Is(err, ErrCredentialEndpointIdentityChanged) {
		t.Fatalf("Finalise error = %v, want identity changed", err)
	}
	if got := replacementDirectory.replacements.Load(); got != 1 {
		t.Fatalf("same-byte receipt replacements = %d, want 1", got)
	}
	if attempts, effects := lease.attempts.Load(), lease.effects.Load(); attempts != 1 || effects != 1 {
		t.Fatalf("lease release attempts/effects = %d/%d, want 1/1", attempts, effects)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(path), ticket.QuarantineName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("receipt-removal replacement retained quarantine: %v", err)
	}
	if _, err := os.Lstat(credentialEndpointMaintenanceRollbackPath(path)); err != nil {
		t.Fatalf("receipt-removal replacement removed receipt: %v", err)
	}
}

func TestOfflineFinaliseRejectsSameByteReceiptReplacementBeforeReceiptRemoval(t *testing.T) {
	t.Parallel()
	path, ticket, endpoint := openActivatedMaintenanceEndpointForFinaliseTest(t)
	trackedDirectory := &finaliseStageDirectory{
		SecureDirectory:   endpoint.secureDirectory,
		recordName:        filepath.Base(credentialEndpointMaintenanceRollbackPath(path)),
		sequence:          &finaliseStageSequence{},
		failReadAfterSync: true,
	}
	endpoint.secureDirectory = trackedDirectory
	generation := endpoint.maintenanceGate.record.Owner.Generation
	if err := endpoint.finaliseActivatedMaintenance(
		context.Background(), ticket, generation,
		legacyMaintenanceFinaliseLeaseVerifier{
			lease: noopLegacyMaintenanceFinaliseLease{},
			verify: func(context.Context, LegacyMaintenanceFinaliseVerification) error {
				trackedDirectory.arm()
				return nil
			},
		},
	); err == nil {
		t.Fatal("fixture finalise unexpectedly completed")
	}
	endpoint.secureDirectory = trackedDirectory.SecureDirectory
	if err := endpoint.Close(); err != nil {
		t.Fatal(err)
	}
	receiptPath := credentialEndpointMaintenanceRollbackPath(path)
	recordData, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	namespace, err := openLegacyCredentialMaintenanceNamespace(path, ticket.Directory)
	if err != nil {
		t.Fatal(err)
	}
	defer namespace.Close()
	directory := &replaceFinalisingReceiptOnReadDirectory{
		SecureDirectory: namespace.directory,
		recordName:      filepath.Base(receiptPath),
		recordData:      recordData,
		armOnProbeAt:    4,
	}
	namespace.directory = directory

	err = finaliseLegacyCredentialEndpointTransitionOfflineInNamespace(context.Background(), path, ticket, namespace)
	if !errors.Is(err, ErrCredentialEndpointIdentityChanged) {
		t.Fatalf("offline Finalise error = %v, want identity changed (replacements=%d probes=%d)", err, directory.replacements.Load(), directory.probeCalls)
	}
	if got := directory.replacements.Load(); got != 1 {
		t.Fatalf("offline same-byte receipt replacements = %d, want 1", got)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(path), ticket.QuarantineName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("offline receipt-removal replacement retained quarantine: %v", err)
	}
	if _, err := os.Lstat(receiptPath); err != nil {
		t.Fatalf("offline receipt-removal replacement removed receipt: %v", err)
	}
}

func openActivatedMaintenanceEndpointForFinaliseTest(t *testing.T) (string, LegacyCredentialEndpointTransitionTicket, *credentialEndpoint) {
	t.Helper()
	path := createRefusedLegacyCredentialSocket(t)
	snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	transition, err := PrepareLegacyCredentialEndpointTransition(
		context.Background(), path, snapshot,
		DrainAuthorityFunc(func(context.Context, string) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	ticket := transition.Ticket()
	if err := transition.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := transition.Close(); err != nil {
		t.Fatal(err)
	}
	endpoint, client, err := openCredentialEndpoint(path, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if client != nil {
		_ = client.Close()
		t.Fatal("opened a delegate instead of the maintenance owner")
	}
	if endpoint.maintenanceGate.record.Owner == nil {
		_ = endpoint.Close()
		t.Fatal("maintenance owner receipt was not bound")
	}
	t.Cleanup(func() { _ = endpoint.Close() })
	return path, ticket, endpoint
}

type finaliseStageSequence struct {
	mu     sync.Mutex
	stages []string
}

func (s *finaliseStageSequence) add(stage string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stages = append(s.stages, stage)
}

func (s *finaliseStageSequence) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.stages...)
}

func finaliseStagesOrdered(stages []string, before, after string) bool {
	beforeIndex := -1
	for index, stage := range stages {
		if stage == before && beforeIndex < 0 {
			beforeIndex = index
		}
		if stage == after && beforeIndex >= 0 {
			return true
		}
	}
	return false
}

type finaliseStageDirectory struct {
	fsutil.SecureDirectory
	recordName              string
	sequence                *finaliseStageSequence
	mu                      sync.Mutex
	armed                   bool
	renamed                 bool
	synced                  bool
	failSync                bool
	failRename              bool
	failReadAfterSync       bool
	readFailureUsed         bool
	failPostReceiptSync     bool
	postReceiptSyncFailed   bool
	postReceiptSyncObserved bool
	afterPostReceiptSync    func()
}

func (d *finaliseStageDirectory) OpenNewExclusiveLock(name string, perm os.FileMode) (fsutil.ExclusiveLock, error) {
	creator, ok := d.SecureDirectory.(fsutil.NewExclusiveLocker)
	if !ok {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	return creator.OpenNewExclusiveLock(name, perm)
}

func (d *finaliseStageDirectory) CreateExclusive(name string, perm os.FileMode) (fsutil.DurableFile, error) {
	file, err := d.SecureDirectory.CreateExclusive(name, perm)
	if err != nil {
		return nil, err
	}
	return &finaliseStageDurableFile{
		DurableFile: file,
		temporary:   name != d.recordName,
		sequence:    d.sequence,
	}, nil
}

func (d *finaliseStageDirectory) ProbeExclusiveLockHeld(name string, perm os.FileMode) (os.FileInfo, error) {
	prober, ok := d.SecureDirectory.(fsutil.ExclusiveLockHeldProber)
	if !ok {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	return prober.ProbeExclusiveLockHeld(name, perm)
}

func (d *finaliseStageDirectory) arm() {
	d.mu.Lock()
	d.armed = true
	d.mu.Unlock()
}

func (d *finaliseStageDirectory) Rename(oldName, newName string) error {
	d.mu.Lock()
	armed := d.armed && newName == d.recordName
	d.mu.Unlock()
	if armed {
		if d.failRename {
			d.sequence.add("rename-error")
			return errors.New("injected finalising receipt rename failure")
		}
		d.sequence.add("rename")
	}
	if err := d.SecureDirectory.Rename(oldName, newName); err != nil {
		return err
	}
	if armed {
		d.mu.Lock()
		d.renamed = true
		d.mu.Unlock()
	}
	return nil
}

func (d *finaliseStageDirectory) RenameChecked(oldName, newName string, expected fsutil.SecureFileIdentity) error {
	d.mu.Lock()
	armed := d.armed && newName == d.recordName
	d.mu.Unlock()
	if armed {
		if d.failRename {
			d.sequence.add("rename-error")
			return errors.New("injected finalising receipt rename failure")
		}
		d.sequence.add("rename")
	}
	if err := d.SecureDirectory.(fsutil.IdentityBoundRenamer).RenameChecked(oldName, newName, expected); err != nil {
		return err
	}
	if armed {
		d.mu.Lock()
		d.renamed = true
		d.mu.Unlock()
	}
	return nil
}

func (d *finaliseStageDirectory) RenameNoReplaceChecked(oldName, newName string, expected fsutil.SecureFileIdentity) error {
	return d.SecureDirectory.(fsutil.IdentityBoundRenamer).RenameNoReplaceChecked(oldName, newName, expected)
}

func (d *finaliseStageDirectory) RemoveChecked(name string, expected fsutil.SecureFileIdentity) error {
	return d.SecureDirectory.(fsutil.IdentityBoundRemover).RemoveChecked(name, expected)
}

func (d *finaliseStageDirectory) Sync() error {
	d.mu.Lock()
	receiptSync := d.armed && d.renamed && !d.synced
	fail := receiptSync && d.failSync
	postReceiptSync := d.armed && d.synced
	failPostReceiptSync := postReceiptSync && d.failPostReceiptSync && !d.postReceiptSyncFailed
	if failPostReceiptSync {
		d.postReceiptSyncFailed = true
	}
	d.mu.Unlock()
	if fail {
		d.sequence.add("sync-error")
		return errors.New("injected finalising receipt directory sync failure")
	}
	if failPostReceiptSync {
		d.sequence.add("post-receipt-sync-error")
		return errors.New("injected post-receipt directory sync failure")
	}
	if err := d.SecureDirectory.Sync(); err != nil {
		return err
	}
	if receiptSync {
		d.mu.Lock()
		d.synced = true
		d.mu.Unlock()
		d.sequence.add("sync")
	} else if postReceiptSync {
		d.mu.Lock()
		first := !d.postReceiptSyncObserved
		d.postReceiptSyncObserved = true
		after := d.afterPostReceiptSync
		d.mu.Unlock()
		if first && after != nil {
			after()
		}
	}
	return nil
}

func (d *finaliseStageDirectory) OpenNoFollow(name string) (fsutil.SecureReadFile, error) {
	file, err := d.SecureDirectory.OpenNoFollow(name)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	readAfterSync := d.armed && d.synced && name == d.recordName
	d.mu.Unlock()
	return &finaliseStageReadFile{
		SecureReadFile: file,
		logRead:        readAfterSync,
		failRead: func() bool {
			if !readAfterSync {
				return false
			}
			d.mu.Lock()
			defer d.mu.Unlock()
			if !d.failReadAfterSync || d.readFailureUsed {
				return false
			}
			d.readFailureUsed = true
			return true
		},
		sequence: d.sequence,
	}, nil
}

type finaliseStageDurableFile struct {
	fsutil.DurableFile
	temporary bool
	sequence  *finaliseStageSequence
}

func (f *finaliseStageDurableFile) Stat() (os.FileInfo, error) {
	inspector, ok := f.DurableFile.(fsutil.DurableFileInspector)
	if !ok {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	return inspector.Stat()
}

func (f *finaliseStageDurableFile) Sync() error {
	if err := f.DurableFile.Sync(); err != nil {
		return err
	}
	if f.temporary {
		f.sequence.add("temp-sync")
	}
	return nil
}

type finaliseStageReadFile struct {
	fsutil.SecureReadFile
	logRead  bool
	failRead func() bool
	logged   bool
	sequence *finaliseStageSequence
}

func (f *finaliseStageReadFile) Read(data []byte) (int, error) {
	if f.failRead != nil && !f.logged && f.failRead() {
		f.logged = true
		f.sequence.add("read-after-sync-error")
		return 0, errors.New("injected finalising receipt read failure")
	}
	count, err := f.SecureReadFile.Read(data)
	if f.logRead && !f.logged && errors.Is(err, io.EOF) {
		f.logged = true
		f.sequence.add("read-after-sync")
	}
	return count, err
}

type finaliseStageLease struct {
	once     sync.Once
	attempts atomic.Int32
	effects  atomic.Int32
	sequence *finaliseStageSequence
}

func (l *finaliseStageLease) Release() {
	l.attempts.Add(1)
	l.once.Do(func() {
		l.effects.Add(1)
		l.sequence.add("release")
	})
}

type finaliseCallbackLease struct {
	once    sync.Once
	calls   atomic.Int32
	release func()
}

func (l *finaliseCallbackLease) Release() {
	l.once.Do(func() {
		l.calls.Add(1)
		if l.release != nil {
			l.release()
		}
	})
}

type replaceFinalisingReceiptOnReadDirectory struct {
	fsutil.SecureDirectory
	recordName   string
	recordData   []byte
	mu           sync.Mutex
	armed        bool
	armOnProbeAt int
	probeCalls   int
	replacements atomic.Int32
}

func (d *replaceFinalisingReceiptOnReadDirectory) RenameChecked(oldName, newName string, expected fsutil.SecureFileIdentity) error {
	return d.SecureDirectory.(fsutil.IdentityBoundRenamer).RenameChecked(oldName, newName, expected)
}

func (d *replaceFinalisingReceiptOnReadDirectory) RenameNoReplaceChecked(oldName, newName string, expected fsutil.SecureFileIdentity) error {
	return d.SecureDirectory.(fsutil.IdentityBoundRenamer).RenameNoReplaceChecked(oldName, newName, expected)
}

func (d *replaceFinalisingReceiptOnReadDirectory) RemoveChecked(name string, expected fsutil.SecureFileIdentity) error {
	return d.SecureDirectory.(fsutil.IdentityBoundRemover).RemoveChecked(name, expected)
}

func (d *replaceFinalisingReceiptOnReadDirectory) arm() {
	d.mu.Lock()
	d.armed = true
	d.mu.Unlock()
}

func (d *replaceFinalisingReceiptOnReadDirectory) OpenNoFollow(name string) (fsutil.SecureReadFile, error) {
	d.mu.Lock()
	replace := d.armed && name == d.recordName
	if replace {
		d.replacements.Add(1)
		d.armed = false
	}
	d.mu.Unlock()
	if replace {
		retained, err := d.SecureDirectory.OpenNoFollow(name)
		if err != nil {
			return nil, err
		}
		defer retained.Close()
		if err := d.SecureDirectory.Remove(name); err != nil {
			return nil, err
		}
		file, err := d.SecureDirectory.CreateExclusive(name, 0o600)
		if err != nil {
			return nil, err
		}
		if _, err := file.Write(d.recordData); err != nil {
			_ = file.Close()
			return nil, err
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return nil, err
		}
		if err := file.Close(); err != nil {
			return nil, err
		}
		if err := d.SecureDirectory.Sync(); err != nil {
			return nil, err
		}
	}
	return d.SecureDirectory.OpenNoFollow(name)
}

func (d *replaceFinalisingReceiptOnReadDirectory) OpenNewExclusiveLock(name string, perm os.FileMode) (fsutil.ExclusiveLock, error) {
	creator, ok := d.SecureDirectory.(fsutil.NewExclusiveLocker)
	if !ok {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	return creator.OpenNewExclusiveLock(name, perm)
}

func (d *replaceFinalisingReceiptOnReadDirectory) ProbeExclusiveLockHeld(name string, perm os.FileMode) (os.FileInfo, error) {
	prober, ok := d.SecureDirectory.(fsutil.ExclusiveLockHeldProber)
	if !ok {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	info, err := prober.ProbeExclusiveLockHeld(name, perm)
	if err == nil {
		d.mu.Lock()
		d.probeCalls++
		if d.armOnProbeAt > 0 && d.probeCalls == d.armOnProbeAt {
			d.armed = true
		}
		d.mu.Unlock()
	}
	return info, err
}

type legacyMaintenanceFinaliseLeaseVerifier struct {
	lease  LegacyMaintenanceFinaliseLease
	err    error
	verify func(context.Context, LegacyMaintenanceFinaliseVerification) error
}

func (v legacyMaintenanceFinaliseLeaseVerifier) AcquireLegacyMaintenanceFinalise(ctx context.Context, proof LegacyMaintenanceFinaliseVerification) (LegacyMaintenanceFinaliseLease, error) {
	if v.verify != nil {
		if err := v.verify(ctx, proof); err != nil {
			return v.lease, err
		}
	}
	return v.lease, v.err
}

type noopLegacyMaintenanceFinaliseLease struct{}

func (noopLegacyMaintenanceFinaliseLease) Release() {}

func testLegacyMaintenanceFinaliseVerifier(verify func(context.Context, LegacyMaintenanceFinaliseVerification) error) LegacyMaintenanceFinaliseVerifier {
	return legacyMaintenanceFinaliseLeaseVerifier{lease: noopLegacyMaintenanceFinaliseLease{}, verify: verify}
}

type blockingLegacyMaintenanceFinaliseLease struct {
	once    sync.Once
	calls   atomic.Int32
	started chan struct{}
	allow   <-chan struct{}
}

func (l *blockingLegacyMaintenanceFinaliseLease) Release() {
	l.once.Do(func() {
		l.calls.Add(1)
		close(l.started)
		<-l.allow
	})
}

func TestCloseBeforeFinaliseLeavesActivatedRollbackWindow(t *testing.T) {
	t.Parallel()
	path := createRefusedLegacyCredentialSocket(t)
	snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	transition, err := PrepareLegacyCredentialEndpointTransition(
		context.Background(), path, snapshot,
		DrainAuthorityFunc(func(context.Context, string) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	ticket := transition.Ticket()
	if err := transition.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := transition.Close(); err != nil {
		t.Fatal(err)
	}
	coordinator, _ := testCoordinator(t)
	owner, err := openCredentialControlPreparedWithLegacyMaintenanceVerifier(
		context.Background(), path, coordinator, false, nil, nil, nil,
		testLegacyMaintenanceFinaliseVerifier(func(context.Context, LegacyMaintenanceFinaliseVerification) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(credentialEndpointMaintenanceRollbackPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if err := FinaliseLegacyCredentialEndpointTransition(context.Background(), path, ticket); !errors.Is(err, ErrCredentialEndpointMaintenanceConflict) {
		t.Fatalf("Finalise after Close error = %v, want conflict", err)
	}
	after, err := os.ReadFile(credentialEndpointMaintenanceRollbackPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("Finalise after Close rewrote activated receipt")
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(path), ticket.QuarantineName)); err != nil {
		t.Fatalf("Finalise after Close removed quarantine: %v", err)
	}
}

func TestConcurrentFinaliseIsIdempotent(t *testing.T) {
	t.Parallel()
	path := createRefusedLegacyCredentialSocket(t)
	snapshot, err := InspectLegacyCredentialEndpoint(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	transition, err := PrepareLegacyCredentialEndpointTransition(
		context.Background(), path, snapshot,
		DrainAuthorityFunc(func(context.Context, string) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	ticket := transition.Ticket()
	if err := transition.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := transition.Close(); err != nil {
		t.Fatal(err)
	}
	coordinator, _ := testCoordinator(t)
	var calls atomic.Int32
	owner, err := openCredentialControlPreparedWithLegacyMaintenanceVerifier(
		context.Background(), path, coordinator, false, nil, nil, nil,
		testLegacyMaintenanceFinaliseVerifier(func(context.Context, LegacyMaintenanceFinaliseVerification) error {
			calls.Add(1)
			return nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			results <- FinaliseLegacyCredentialEndpointTransition(context.Background(), path, ticket)
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("concurrent Finalise error = %v", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("verifier calls = %d, want 1", got)
	}
}

func TestRollbackRecordPresenceBlocksPristineInspectAndOrdinaryOpen(t *testing.T) {
	t.Parallel()
	path := createRefusedLegacyCredentialSocket(t)
	rollbackPath := path + ".maintenance.rollback.json"
	if err := os.WriteFile(rollbackPath, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := InspectLegacyCredentialEndpoint(context.Background(), path); !errors.Is(err, ErrLegacyCredentialEndpointArtifacts) {
		t.Fatalf("inspect error = %v, want maintenance artifacts", err)
	}
	endpoint, client, err := openCredentialEndpoint(path, false, nil)
	if endpoint != nil {
		_ = endpoint.Close()
	}
	if client != nil {
		_ = client.Close()
	}
	if !errors.Is(err, ErrCredentialEndpointMaintenancePending) {
		t.Fatalf("ordinary open error = %v, want maintenance pending", err)
	}
}

func TestCredentialEndpointOpenFailureDoesNotExposeOwnerGeneration(t *testing.T) {
	t.Parallel()
	path := filepath.Join(shortEndpointDir(t), "credential.sock")
	const generation = "SECRET_OWNER_GENERATION_CANARY"
	var injected sync.Once
	hook := func(phase credentialEndpointPhase) {
		if phase != credentialEndpointPhaseLifetimeLockAcquired {
			return
		}
		injected.Do(func() {
			info, err := os.Lstat(credentialEndpointLockPath(path))
			if err != nil {
				t.Fatal(err)
			}
			identity, ok := (fsutil.OSFileSystem{}).FileIdentity(info)
			if !ok {
				t.Fatal("lock identity unavailable")
			}
			sidecar := credentialEndpointSidecar{
				Version: credentialEndpointSidecarVersion, ProtocolVersion: credentialEndpointProtocolVersion,
				Generation: generation, State: credentialEndpointPrepared,
				TemporaryName: ".cq-credential-canary.sock", FinalName: filepath.Base(path),
				credentialEndpointIdentity: credentialEndpointIdentity{
					Device: 1, Inode: 2, UID: uint64(os.Geteuid()), Type: "socket", Mode: 0o600,
				},
				LockDevice: identity.Device, LockInode: identity.Inode, LockLinks: identity.Links,
			}
			data, err := json.Marshal(sidecar)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(credentialEndpointSidecarPath(path), data, 0o600); err != nil {
				t.Fatal(err)
			}
		})
	}
	endpoint, client, err := openCredentialEndpoint(path, false, hook)
	if endpoint != nil {
		_ = endpoint.Close()
	}
	if client != nil {
		_ = client.Close()
	}
	if err == nil {
		t.Fatal("open unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), generation) {
		t.Fatalf("open error exposed owner generation: %v", err)
	}
}
