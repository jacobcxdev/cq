//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package codex

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"golang.org/x/sys/unix"
)

type legacyCredentialMaintenanceNamespace struct {
	fs             fsutil.OSFileSystem
	directory      fsutil.SecureDirectory
	directoryFD    int
	directoryPath  string
	directoryProof LegacyCredentialEndpointIdentity
	path           string
	finalName      string
	lockName       string
	journalName    string
}

type legacyCredentialEndpointTransition struct {
	mu        sync.Mutex
	namespace *legacyCredentialMaintenanceNamespace
	lock      fsutil.ExclusiveLock
	ticket    LegacyCredentialEndpointTransitionTicket
	journal   credentialEndpointMaintenanceJournal
	authority DrainAuthority
	closed    bool
}

type legacyCredentialMaintenanceLock struct {
	mu     sync.Mutex
	file   *os.File
	closed bool
}

func (lock *legacyCredentialMaintenanceLock) Stat() (os.FileInfo, error) {
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.closed {
		return nil, os.ErrClosed
	}
	return lock.file.Stat()
}

func (lock *legacyCredentialMaintenanceLock) Close() error {
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.closed {
		return nil
	}
	lock.closed = true
	return errors.Join(unix.Flock(int(lock.file.Fd()), unix.LOCK_UN), lock.file.Close())
}

func PrepareLegacyCredentialEndpointTransition(ctx context.Context, path string, snapshot LegacyCredentialEndpointSnapshot, authority DrainAuthority) (*LegacyCredentialEndpointTransition, error) {
	if err := assertLegacyCredentialDrainAuthority(ctx, path, authority); err != nil {
		return nil, err
	}
	if err := snapshot.validate(); err != nil || snapshot.Path != path {
		return nil, errors.Join(ErrCredentialEndpointMaintenanceSnapshotChanged, err)
	}
	namespace, err := openLegacyCredentialMaintenanceNamespace(path, snapshot.Directory)
	if err != nil {
		return nil, err
	}
	closeNamespace := true
	defer func() {
		if closeNamespace {
			_ = namespace.Close()
		}
	}()
	lock, lockProof, err := namespace.acquirePrepareLockAndProbe(ctx, snapshot, authority)
	if err != nil {
		return nil, err
	}
	transition := &legacyCredentialEndpointTransition{namespace: namespace, lock: lock, authority: authority}
	closeNamespace = false
	closeTransition := true
	defer func() {
		if closeTransition {
			_ = transition.Close()
		}
	}()
	ticketID, err := newLegacyCredentialEndpointTicketID()
	if err != nil {
		return nil, err
	}
	ticket := LegacyCredentialEndpointTransitionTicket{
		Version: legacyCredentialEndpointTransitionVersion,
		ID:      ticketID, Path: path,
		Directory: snapshot.Directory, Socket: snapshot.Socket, Lock: lockProof,
		QuarantineName: "." + filepath.Base(path) + ".legacy-" + ticketID + ".quarantine",
	}
	ticketHash, err := credentialEndpointMaintenanceTicketHash(ticket)
	if err != nil {
		return nil, err
	}
	journal := credentialEndpointMaintenanceJournal{
		Version:    credentialEndpointMaintenanceJournalVersion,
		Generation: 1,
		State:      CredentialEndpointMaintenancePrepared,
		TicketHash: ticketHash,
		Ticket:     ticket,
	}
	transition.ticket = ticket
	transition.journal = journal
	if err := namespace.requireEntryAbsent(ticket.QuarantineName); err != nil {
		return nil, fmt.Errorf("validate quarantine absence: %w", err)
	}
	if err := assertLegacyCredentialDrainAuthority(ctx, path, authority); err != nil {
		return nil, err
	}
	writeErr := namespace.writeJournal(journal, func() error {
		if err := namespace.validateLegacySnapshot(snapshot, &lockProof); err != nil {
			return err
		}
		return namespace.requireEntryAbsent(ticket.QuarantineName)
	})
	if writeErr != nil {
		return nil, fmt.Errorf("write prepared maintenance journal: %w", writeErr)
	}

	if err := namespace.detachSocket(ctx, ticket, journal, authority); err != nil {
		return nil, fmt.Errorf("quarantine legacy socket: %w", err)
	}
	if err := assertLegacyCredentialDrainAuthority(ctx, path, authority); err != nil {
		return nil, err
	}
	quarantined := journal
	quarantined.Generation++
	quarantined.State = CredentialEndpointMaintenanceQuarantined
	if err := namespace.writeJournal(quarantined, func() error {
		if err := namespace.validateJournal(journal); err != nil {
			return err
		}
		return namespace.validateQuarantined(ticket)
	}); err != nil {
		return nil, fmt.Errorf("write quarantined maintenance journal: %w", err)
	}
	transition.journal = quarantined
	closeTransition = false
	return &LegacyCredentialEndpointTransition{implementation: transition}, nil
}

func (namespace *legacyCredentialMaintenanceNamespace) acquirePrepareLockAndProbe(ctx context.Context, snapshot LegacyCredentialEndpointSnapshot, authority DrainAuthority) (fsutil.ExclusiveLock, LegacyCredentialEndpointIdentity, error) {
	existing, exists, err := namespace.inspectOptionalRegular(namespace.lockName)
	if err != nil {
		return nil, LegacyCredentialEndpointIdentity{}, err
	}
	var lock fsutil.ExclusiveLock
	if exists {
		lock, err = namespace.openExistingLock(existing)
		if err != nil {
			return nil, LegacyCredentialEndpointIdentity{}, err
		}
	} else {
		if err := namespace.validateLegacySnapshot(snapshot, nil); err != nil {
			return nil, LegacyCredentialEndpointIdentity{}, fmt.Errorf("validate legacy snapshot before first probe: %w", err)
		}
		if err := namespace.probeRefused(ctx); err != nil {
			return nil, LegacyCredentialEndpointIdentity{}, fmt.Errorf("first refused probe: %w", err)
		}
		if err := namespace.validateLegacySnapshot(snapshot, nil); err != nil {
			return nil, LegacyCredentialEndpointIdentity{}, fmt.Errorf("validate legacy snapshot after first probe: %w", err)
		}
		lock, err = fsutil.AcquireNewExclusiveLockInDirectory(namespace.fs, namespace.directory, namespace.lockName)
		if err != nil {
			return nil, LegacyCredentialEndpointIdentity{}, err
		}
	}
	closeLock := true
	defer func() {
		if closeLock {
			_ = lock.Close()
		}
	}()
	lockProof, err := namespace.inspectHeldLock(lock)
	if err != nil {
		return nil, LegacyCredentialEndpointIdentity{}, fmt.Errorf("inspect prepare lock: %w", err)
	}
	if exists && lockProof != existing {
		return nil, LegacyCredentialEndpointIdentity{}, ErrCredentialEndpointIdentityChanged
	}
	if err := namespace.validateLegacySnapshot(snapshot, &lockProof); err != nil {
		return nil, LegacyCredentialEndpointIdentity{}, fmt.Errorf("validate legacy snapshot after lock: %w", err)
	}
	if err := assertLegacyCredentialDrainAuthority(ctx, snapshot.Path, authority); err != nil {
		return nil, LegacyCredentialEndpointIdentity{}, err
	}
	if exists {
		if err := namespace.probeRefused(ctx); err != nil {
			return nil, LegacyCredentialEndpointIdentity{}, fmt.Errorf("first adopted-lock refused probe: %w", err)
		}
	}
	timer := time.NewTimer(5 * time.Millisecond)
	select {
	case <-ctx.Done():
		if !timer.Stop() {
			<-timer.C
		}
		return nil, LegacyCredentialEndpointIdentity{}, ctx.Err()
	case <-timer.C:
	}
	if err := assertLegacyCredentialDrainAuthority(ctx, snapshot.Path, authority); err != nil {
		return nil, LegacyCredentialEndpointIdentity{}, err
	}
	if err := namespace.probeRefused(ctx); err != nil {
		return nil, LegacyCredentialEndpointIdentity{}, fmt.Errorf("second refused probe: %w", err)
	}
	if err := namespace.validateLegacySnapshot(snapshot, &lockProof); err != nil {
		return nil, LegacyCredentialEndpointIdentity{}, fmt.Errorf("validate legacy snapshot after second probe: %w", err)
	}
	closeLock = false
	return lock, lockProof, nil
}

func InspectLegacyCredentialEndpointTransition(ctx context.Context, path string) (LegacyCredentialEndpointTransitionStatus, error) {
	if err := ctx.Err(); err != nil {
		return LegacyCredentialEndpointTransitionStatus{}, err
	}
	directoryPath, finalName, err := validateCredentialEndpointPath(path)
	if err != nil {
		return LegacyCredentialEndpointTransitionStatus{}, err
	}
	fsys := fsutil.OSFileSystem{}
	if err := fsutil.ValidateSecureDirectory(fsys, directoryPath); err != nil {
		return LegacyCredentialEndpointTransitionStatus{}, err
	}
	directory, err := fsys.OpenSecureDirectory(directoryPath)
	if err != nil {
		return LegacyCredentialEndpointTransitionStatus{}, err
	}
	directoryFD, err := openCredentialEndpointDirectory(directoryPath)
	if err != nil {
		_ = directory.Close()
		return LegacyCredentialEndpointTransitionStatus{}, err
	}
	namespace := &legacyCredentialMaintenanceNamespace{
		fs: fsys, directory: directory, directoryFD: directoryFD,
		directoryPath: directoryPath, path: path, finalName: finalName,
		lockName:    filepath.Base(credentialEndpointLockPath(path)),
		journalName: filepath.Base(credentialEndpointMaintenanceJournalPath(path)),
	}
	defer namespace.Close()
	proof, err := inspectLegacyCredentialDirectory(fsys, directory, directoryFD, directoryPath)
	if err != nil {
		return LegacyCredentialEndpointTransitionStatus{}, err
	}
	namespace.directoryProof = proof
	journal, err := namespace.readJournal()
	if err != nil {
		return LegacyCredentialEndpointTransitionStatus{}, err
	}
	if journal.Ticket.Path != path || legacyCredentialEndpointDirectoryDifference(proof, journal.Ticket.Directory) != "" {
		return LegacyCredentialEndpointTransitionStatus{}, ErrCredentialEndpointMaintenanceTicketMismatch
	}
	lock, err := namespace.inspectRegular(namespace.lockName)
	if err != nil || lock != journal.Ticket.Lock {
		return LegacyCredentialEndpointTransitionStatus{}, errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
	}
	if err := namespace.validateReadOnlyJournalShape(journal); err != nil {
		return LegacyCredentialEndpointTransitionStatus{}, err
	}
	return LegacyCredentialEndpointTransitionStatus{State: journal.State, Ticket: journal.Ticket}, nil
}

func ResumeLegacyCredentialEndpointTransition(ctx context.Context, path string, ticket LegacyCredentialEndpointTransitionTicket, authority DrainAuthority) (*LegacyCredentialEndpointTransition, error) {
	if err := assertLegacyCredentialDrainAuthority(ctx, path, authority); err != nil {
		return nil, err
	}
	if err := ticket.validate(); err != nil || ticket.Path != path {
		return nil, errors.Join(ErrCredentialEndpointMaintenanceTicketMismatch, err)
	}
	namespace, err := openLegacyCredentialMaintenanceNamespace(path, ticket.Directory)
	if err != nil {
		return nil, err
	}
	closeNamespace := true
	defer func() {
		if closeNamespace {
			_ = namespace.Close()
		}
	}()
	journal, err := namespace.readJournal()
	if err != nil || journal.Ticket != ticket {
		return nil, errors.Join(ErrCredentialEndpointMaintenanceTicketMismatch, err)
	}
	lockProof, err := namespace.inspectRegular(namespace.lockName)
	if err != nil || lockProof != ticket.Lock {
		return nil, errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
	}
	lock, err := namespace.openExistingLock(ticket.Lock)
	if err != nil {
		return nil, err
	}
	transition := &legacyCredentialEndpointTransition{
		namespace: namespace, lock: lock, ticket: ticket, journal: journal, authority: authority,
	}
	closeNamespace = false
	closeTransition := true
	defer func() {
		if closeTransition {
			_ = transition.Close()
		}
	}()
	current, err := namespace.readExpectedJournal(ticket)
	if err != nil || current != journal {
		return nil, errors.Join(ErrCredentialEndpointMaintenanceTicketMismatch, err)
	}
	if err := assertLegacyCredentialDrainAuthority(ctx, path, authority); err != nil {
		return nil, err
	}
	public := &LegacyCredentialEndpointTransition{implementation: transition}
	switch journal.State {
	case CredentialEndpointMaintenancePrepared:
		if err := transition.resumePrepared(ctx); err != nil {
			return nil, err
		}
	case CredentialEndpointMaintenanceQuarantined:
		if err := namespace.validateQuarantinedWithJournal(ticket, journal); err != nil {
			return nil, err
		}
	case CredentialEndpointMaintenanceCommitting:
		if err := public.Commit(ctx); err != nil {
			return nil, err
		}
	case CredentialEndpointMaintenanceRollingBack:
		if err := public.Rollback(ctx); err != nil {
			return nil, err
		}
	case CredentialEndpointMaintenanceRolledBack:
		if err := transition.resumeRolledBack(ctx); err != nil {
			return nil, err
		}
	default:
		return nil, ErrCredentialEndpointMaintenanceConflict
	}
	closeTransition = false
	return public, nil
}

func (transition *legacyCredentialEndpointTransition) Ticket() LegacyCredentialEndpointTransitionTicket {
	if transition == nil {
		return LegacyCredentialEndpointTransitionTicket{}
	}
	return transition.ticket
}

func (transition *legacyCredentialEndpointTransition) State() CredentialEndpointMaintenanceState {
	if transition == nil {
		return ""
	}
	transition.mu.Lock()
	defer transition.mu.Unlock()
	return transition.journal.State
}

func (transition *legacyCredentialEndpointTransition) Commit(ctx context.Context) error {
	if transition == nil {
		return ErrCredentialEndpointMaintenanceConflict
	}
	transition.mu.Lock()
	defer transition.mu.Unlock()
	if transition.closed {
		return ErrCredentialEndpointMaintenanceConflict
	}
	if transition.journal.State == CredentialEndpointMaintenanceCommitted {
		return nil
	}
	if err := assertLegacyCredentialDrainAuthority(ctx, transition.ticket.Path, transition.authority); err != nil {
		return err
	}
	journal, err := transition.namespace.readExpectedJournal(transition.ticket)
	if err != nil {
		return err
	}
	switch journal.State {
	case CredentialEndpointMaintenanceQuarantined:
		if err := transition.namespace.validateQuarantinedWithJournal(transition.ticket, journal); err != nil {
			return err
		}
		if err := assertLegacyCredentialDrainAuthority(ctx, transition.ticket.Path, transition.authority); err != nil {
			return err
		}
		committing := journal
		committing.Generation++
		committing.State = CredentialEndpointMaintenanceCommitting
		if err := transition.namespace.writeJournal(committing, func() error {
			if err := transition.namespace.validateJournal(journal); err != nil {
				return err
			}
			return transition.namespace.validateQuarantined(transition.ticket)
		}); err != nil {
			return fmt.Errorf("write committing maintenance journal: %w", err)
		}
		journal = committing
		transition.journal = committing
	case CredentialEndpointMaintenanceCommitting:
		transition.journal = journal
	default:
		return fmt.Errorf("%w: cannot commit state %q", ErrCredentialEndpointMaintenanceConflict, journal.State)
	}
	if err := assertLegacyCredentialDrainAuthority(ctx, transition.ticket.Path, transition.authority); err != nil {
		return err
	}
	if err := transition.namespace.removeQuarantineForCommit(transition.ticket, journal); err != nil {
		return err
	}
	if err := assertLegacyCredentialDrainAuthority(ctx, transition.ticket.Path, transition.authority); err != nil {
		return err
	}
	if err := transition.namespace.removeJournalForCommit(transition.ticket, journal); err != nil {
		return err
	}
	transition.journal.State = CredentialEndpointMaintenanceCommitted
	return nil
}

func (transition *legacyCredentialEndpointTransition) Rollback(ctx context.Context) error {
	if transition == nil {
		return ErrCredentialEndpointMaintenanceConflict
	}
	transition.mu.Lock()
	defer transition.mu.Unlock()
	if transition.closed {
		return ErrCredentialEndpointMaintenanceConflict
	}
	if err := assertLegacyCredentialDrainAuthority(ctx, transition.ticket.Path, transition.authority); err != nil {
		return err
	}
	journal, err := transition.namespace.readExpectedJournal(transition.ticket)
	if err != nil {
		return err
	}
	switch journal.State {
	case CredentialEndpointMaintenanceRolledBack:
		if err := transition.namespace.validateRolledBackWithJournal(transition.ticket, journal); err != nil {
			return err
		}
		transition.journal = journal
		return nil
	case CredentialEndpointMaintenanceQuarantined:
		if err := transition.namespace.validateQuarantinedWithJournal(transition.ticket, journal); err != nil {
			return err
		}
		if err := assertLegacyCredentialDrainAuthority(ctx, transition.ticket.Path, transition.authority); err != nil {
			return err
		}
		rollingBack := journal
		rollingBack.Generation++
		rollingBack.State = CredentialEndpointMaintenanceRollingBack
		if err := transition.namespace.writeJournal(rollingBack, func() error {
			if err := transition.namespace.validateJournal(journal); err != nil {
				return err
			}
			return transition.namespace.validateQuarantined(transition.ticket)
		}); err != nil {
			return fmt.Errorf("write rolling-back maintenance journal: %w", err)
		}
		journal = rollingBack
		transition.journal = rollingBack
	case CredentialEndpointMaintenanceRollingBack:
		transition.journal = journal
	default:
		return fmt.Errorf("%w: cannot roll back state %q", ErrCredentialEndpointMaintenanceConflict, journal.State)
	}
	if err := transition.namespace.restoreSocketForRollback(ctx, transition.ticket, journal, transition.authority); err != nil {
		return err
	}
	if err := assertLegacyCredentialDrainAuthority(ctx, transition.ticket.Path, transition.authority); err != nil {
		return err
	}
	rolledBack := journal
	rolledBack.Generation++
	rolledBack.State = CredentialEndpointMaintenanceRolledBack
	if err := transition.namespace.writeJournal(rolledBack, func() error {
		if err := transition.namespace.validateJournal(journal); err != nil {
			return err
		}
		return transition.namespace.validateRolledBack(transition.ticket)
	}); err != nil {
		return fmt.Errorf("write rolled-back maintenance journal: %w", err)
	}
	transition.journal = rolledBack
	return nil
}

func (transition *legacyCredentialEndpointTransition) resumePrepared(ctx context.Context) error {
	journal := transition.journal
	if err := transition.namespace.validateReadOnlyJournalShape(journal); err != nil {
		return err
	}
	_, finalExists, err := transition.namespace.inspectOptionalSocket(transition.namespace.finalName)
	if err != nil {
		return err
	}
	_, quarantineExists, err := transition.namespace.inspectOptionalSocket(transition.ticket.QuarantineName)
	if err != nil {
		return err
	}
	if finalExists && !quarantineExists {
		if err := transition.doubleProbeRolledBack(ctx, journal); err != nil {
			return err
		}
	}
	if err := transition.namespace.detachSocket(ctx, transition.ticket, journal, transition.authority); err != nil {
		return err
	}
	return transition.finishQuarantinedJournal(ctx, journal)
}

func (transition *legacyCredentialEndpointTransition) resumeRolledBack(ctx context.Context) error {
	journal := transition.journal
	if err := transition.namespace.validateRolledBackWithJournal(transition.ticket, journal); err != nil {
		return err
	}
	if err := transition.doubleProbeRolledBack(ctx, journal); err != nil {
		return err
	}
	if err := assertLegacyCredentialDrainAuthority(ctx, transition.ticket.Path, transition.authority); err != nil {
		return err
	}
	prepared := journal
	prepared.Generation++
	prepared.State = CredentialEndpointMaintenancePrepared
	if err := transition.namespace.writeJournal(prepared, func() error {
		if err := transition.namespace.validateJournal(journal); err != nil {
			return err
		}
		return transition.namespace.validateRolledBack(transition.ticket)
	}); err != nil {
		return fmt.Errorf("write re-detach maintenance journal: %w", err)
	}
	transition.journal = prepared
	if err := transition.namespace.detachSocket(ctx, transition.ticket, prepared, transition.authority); err != nil {
		return err
	}
	return transition.finishQuarantinedJournal(ctx, prepared)
}

func (transition *legacyCredentialEndpointTransition) doubleProbeRolledBack(ctx context.Context, journal credentialEndpointMaintenanceJournal) error {
	for probe := 0; probe < 2; probe++ {
		if err := assertLegacyCredentialDrainAuthority(ctx, transition.ticket.Path, transition.authority); err != nil {
			return err
		}
		if err := transition.namespace.validateJournal(journal); err != nil {
			return err
		}
		if err := transition.namespace.validateRolledBack(transition.ticket); err != nil {
			return err
		}
		if err := transition.namespace.probeRefused(ctx); err != nil {
			return err
		}
		if probe == 0 {
			timer := time.NewTimer(5 * time.Millisecond)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	return nil
}

func (transition *legacyCredentialEndpointTransition) finishQuarantinedJournal(ctx context.Context, previous credentialEndpointMaintenanceJournal) error {
	if err := assertLegacyCredentialDrainAuthority(ctx, transition.ticket.Path, transition.authority); err != nil {
		return err
	}
	quarantined := previous
	quarantined.Generation++
	quarantined.State = CredentialEndpointMaintenanceQuarantined
	if err := transition.namespace.writeJournal(quarantined, func() error {
		if err := transition.namespace.validateJournal(previous); err != nil {
			return err
		}
		return transition.namespace.validateQuarantined(transition.ticket)
	}); err != nil {
		return fmt.Errorf("write quarantined maintenance journal: %w", err)
	}
	transition.journal = quarantined
	return nil
}

func (transition *legacyCredentialEndpointTransition) Close() error {
	if transition == nil {
		return nil
	}
	transition.mu.Lock()
	defer transition.mu.Unlock()
	if transition.closed {
		return nil
	}
	transition.closed = true
	var lockErr error
	if transition.lock != nil {
		lockErr = transition.lock.Close()
	}
	return errors.Join(lockErr, transition.namespace.Close())
}

func assertLegacyCredentialDrainAuthority(ctx context.Context, path string, authority DrainAuthority) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if authority == nil {
		return ErrCredentialEndpointMaintenanceDrainRequired
	}
	if err := authority.AssertStoppedAndDrained(ctx, path); err != nil {
		return errors.Join(ErrCredentialEndpointMaintenanceDrainRequired, err)
	}
	return ctx.Err()
}

func openLegacyCredentialMaintenanceNamespace(path string, expectedDirectory LegacyCredentialEndpointIdentity) (*legacyCredentialMaintenanceNamespace, error) {
	directoryPath, finalName, err := validateCredentialEndpointPath(path)
	if err != nil {
		return nil, err
	}
	fsys := fsutil.OSFileSystem{}
	if err := fsutil.ValidateSecureDirectory(fsys, directoryPath); err != nil {
		return nil, err
	}
	directory, err := fsys.OpenSecureDirectory(directoryPath)
	if err != nil {
		return nil, err
	}
	directoryFD, err := openCredentialEndpointDirectory(directoryPath)
	if err != nil {
		_ = directory.Close()
		return nil, err
	}
	namespace := &legacyCredentialMaintenanceNamespace{
		fs: fsys, directory: directory, directoryFD: directoryFD,
		directoryPath: directoryPath, path: path, finalName: finalName,
		lockName:    filepath.Base(credentialEndpointLockPath(path)),
		journalName: filepath.Base(credentialEndpointMaintenanceJournalPath(path)),
	}
	proof, err := inspectLegacyCredentialDirectory(fsys, directory, directoryFD, directoryPath)
	if err != nil {
		_ = namespace.Close()
		return nil, errors.Join(ErrCredentialEndpointMaintenanceSnapshotChanged, err)
	}
	if difference := legacyCredentialEndpointDirectoryDifference(proof, expectedDirectory); difference != "" {
		_ = namespace.Close()
		return nil, fmt.Errorf("%w: directory %s changed", ErrCredentialEndpointMaintenanceSnapshotChanged, difference)
	}
	namespace.directoryProof = proof
	return namespace, nil
}

func legacyCredentialEndpointDirectoryDifference(actual, expected LegacyCredentialEndpointIdentity) string {
	expected.Links = actual.Links
	return legacyCredentialEndpointIdentityDifference(actual, expected)
}

func legacyCredentialEndpointIdentityDifference(actual, expected LegacyCredentialEndpointIdentity) string {
	switch {
	case actual.Device != expected.Device:
		return "device"
	case actual.Inode != expected.Inode:
		return "inode"
	case actual.UID != expected.UID:
		return "owner"
	case actual.Links != expected.Links:
		return "links"
	case actual.Type != expected.Type:
		return "type"
	case actual.Mode != expected.Mode:
		return "mode"
	default:
		return ""
	}
}

func (namespace *legacyCredentialMaintenanceNamespace) Close() error {
	if namespace == nil {
		return nil
	}
	fdErr := unix.Close(namespace.directoryFD)
	namespace.directoryFD = -1
	return errors.Join(fdErr, namespace.directory.Close())
}

func (namespace *legacyCredentialMaintenanceNamespace) validateDirectory() error {
	proof, err := inspectLegacyCredentialDirectory(namespace.fs, namespace.directory, namespace.directoryFD, namespace.directoryPath)
	if err != nil {
		return errors.Join(ErrCredentialEndpointMaintenanceSnapshotChanged, err)
	}
	// APFS changes a directory's reported link count as entries are added and
	// removed. The retained descriptor and pathname are still bound by device
	// and inode; ownership, type, and mode remain security invariants.
	expected := namespace.directoryProof
	expected.Links = proof.Links
	if difference := legacyCredentialEndpointIdentityDifference(proof, expected); difference != "" {
		return fmt.Errorf("%w: directory %s changed", ErrCredentialEndpointMaintenanceSnapshotChanged, difference)
	}
	return nil
}

func (namespace *legacyCredentialMaintenanceNamespace) validateLegacySnapshot(snapshot LegacyCredentialEndpointSnapshot, expectedLock *LegacyCredentialEndpointIdentity) error {
	if err := namespace.validateDirectory(); err != nil {
		return err
	}
	proof, err := namespace.inspectSocket(namespace.finalName, 1)
	if err != nil || proof != snapshot.Socket || legacyCredentialEndpointDirectoryDifference(namespace.directoryProof, snapshot.Directory) != "" || snapshot.Path != namespace.path {
		return errors.Join(ErrCredentialEndpointMaintenanceSnapshotChanged, err)
	}
	if err := namespace.requireEntryAbsent(filepath.Base(credentialEndpointSidecarPath(namespace.path))); err != nil {
		return errors.Join(ErrLegacyCredentialEndpointArtifacts, err)
	}
	if err := namespace.requireEntryAbsent(namespace.journalName); err != nil {
		return errors.Join(ErrLegacyCredentialEndpointArtifacts, err)
	}
	if expectedLock == nil {
		if err := namespace.requireEntryAbsent(namespace.lockName); err != nil {
			return errors.Join(ErrLegacyCredentialEndpointArtifacts, err)
		}
	} else if err := namespace.validateHeldLock(*expectedLock); err != nil {
		return err
	}
	return namespace.validateDirectory()
}

func (namespace *legacyCredentialMaintenanceNamespace) probeRefused(ctx context.Context) error {
	if err := namespace.validateDirectory(); err != nil {
		return err
	}
	dialer := net.Dialer{Timeout: credentialEndpointDialTimeout}
	conn, err := dialer.DialContext(ctx, "unix", namespace.path)
	if conn != nil {
		_ = conn.Close()
		return ErrLegacyCredentialEndpointNotRefused
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return errors.Join(ErrLegacyCredentialEndpointNotRefused, err)
	}
	return namespace.validateDirectory()
}

func (namespace *legacyCredentialMaintenanceNamespace) inspectHeldLock(lock fsutil.ExclusiveLock) (LegacyCredentialEndpointIdentity, error) {
	if lock == nil {
		return LegacyCredentialEndpointIdentity{}, fsutil.ErrExclusiveLockNotHeld
	}
	info, err := lock.Stat()
	if err != nil {
		return LegacyCredentialEndpointIdentity{}, err
	}
	descriptorID, ok := namespace.fs.FileIdentity(info)
	if !ok {
		return LegacyCredentialEndpointIdentity{}, fsutil.ErrUnsafeSecurePath
	}
	proof, err := namespace.inspectRegular(namespace.lockName)
	if err != nil {
		return LegacyCredentialEndpointIdentity{}, err
	}
	if descriptorID.Device != proof.Device || descriptorID.Inode != proof.Inode || descriptorID.Links != proof.Links {
		return LegacyCredentialEndpointIdentity{}, ErrCredentialEndpointIdentityChanged
	}
	if err := namespace.validateHeldLock(proof); err != nil {
		return LegacyCredentialEndpointIdentity{}, err
	}
	return proof, nil
}

func (namespace *legacyCredentialMaintenanceNamespace) openExistingLock(expected LegacyCredentialEndpointIdentity) (fsutil.ExclusiveLock, error) {
	fd, err := unix.Openat(namespace.directoryFD, namespace.lockName, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
	}
	closeFD := true
	defer func() {
		if closeFD {
			_ = unix.Close(fd)
		}
	}()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	actual := LegacyCredentialEndpointIdentity{
		Device: uint64(stat.Dev), Inode: uint64(stat.Ino), UID: uint64(stat.Uid), Links: uint64(stat.Nlink),
		Type: "regular", Mode: uint32(stat.Mode) & 0o777,
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&(unix.S_ISUID|unix.S_ISGID|unix.S_ISVTX) != 0 || actual != expected {
		return nil, ErrCredentialEndpointIdentityChanged
	}
	if named, err := namespace.inspectRegular(namespace.lockName); err != nil || named != expected {
		return nil, errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, fsutil.ErrExclusiveLockHeld
		}
		return nil, err
	}
	file := os.NewFile(uintptr(fd), namespace.lockName)
	if file == nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	closeFD = false
	lock := &legacyCredentialMaintenanceLock{file: file}
	if proof, err := namespace.inspectHeldLock(lock); err != nil || proof != expected {
		_ = lock.Close()
		return nil, errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	return lock, nil
}

func (namespace *legacyCredentialMaintenanceNamespace) validateHeldLock(expected LegacyCredentialEndpointIdentity) error {
	proof, err := namespace.inspectRegular(namespace.lockName)
	if err != nil || proof != expected {
		return errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	identity := fsutil.SecureFileIdentity{Device: expected.Device, Inode: expected.Inode, Links: expected.Links}
	if err := fsutil.ValidateExclusiveLockHeldInDirectory(namespace.fs, namespace.directory, namespace.lockName, identity); err != nil {
		return err
	}
	return namespace.validateDirectory()
}

func (namespace *legacyCredentialMaintenanceNamespace) inspectRegular(name string) (LegacyCredentialEndpointIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(namespace.directoryFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return LegacyCredentialEndpointIdentity{}, err
	}
	proof := LegacyCredentialEndpointIdentity{
		Device: uint64(stat.Dev), Inode: uint64(stat.Ino), UID: uint64(stat.Uid), Links: uint64(stat.Nlink),
		Type: "regular", Mode: uint32(stat.Mode) & 0o777,
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&(unix.S_ISUID|unix.S_ISGID|unix.S_ISVTX) != 0 || proof.UID != uint64(os.Geteuid()) || proof.Mode != 0o600 || proof.Links != 1 {
		return LegacyCredentialEndpointIdentity{}, fsutil.ErrUnsafeSecurePath
	}
	return proof, nil
}

func (namespace *legacyCredentialMaintenanceNamespace) inspectOptionalRegular(name string) (LegacyCredentialEndpointIdentity, bool, error) {
	proof, err := namespace.inspectRegular(name)
	if errors.Is(err, unix.ENOENT) {
		return LegacyCredentialEndpointIdentity{}, false, nil
	}
	if err != nil {
		return LegacyCredentialEndpointIdentity{}, false, err
	}
	return proof, true, nil
}

func (namespace *legacyCredentialMaintenanceNamespace) inspectSocket(name string, links uint64) (LegacyCredentialEndpointIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(namespace.directoryFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return LegacyCredentialEndpointIdentity{}, err
	}
	proof := LegacyCredentialEndpointIdentity{
		Device: uint64(stat.Dev), Inode: uint64(stat.Ino), UID: uint64(stat.Uid), Links: uint64(stat.Nlink),
		Type: "socket", Mode: uint32(stat.Mode) & 0o777,
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK || stat.Mode&(unix.S_ISUID|unix.S_ISGID|unix.S_ISVTX) != 0 || proof.UID != uint64(os.Geteuid()) || proof.Mode != 0o600 || proof.Links != links {
		return LegacyCredentialEndpointIdentity{}, ErrCredentialEndpointIdentityChanged
	}
	return proof, nil
}

func (namespace *legacyCredentialMaintenanceNamespace) requireEntryAbsent(name string) error {
	var stat unix.Stat_t
	err := unix.Fstatat(namespace.directoryFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	return &os.PathError{Op: "inspect", Path: name, Err: os.ErrExist}
}

func (namespace *legacyCredentialMaintenanceNamespace) writeJournal(journal credentialEndpointMaintenanceJournal, precondition func() error) error {
	data, err := encodeCredentialEndpointMaintenanceJournal(journal)
	if err != nil {
		return err
	}
	if err := fsutil.SecureAtomicWriteInDirectoryChecked(namespace.fs, namespace.directory, namespace.journalName, data, func() error {
		if err := namespace.validateDirectory(); err != nil {
			return err
		}
		return precondition()
	}); err != nil {
		return err
	}
	read, _, err := fsutil.ReadSecureFileInDirectoryWithIdentity(namespace.fs, namespace.directory, namespace.journalName, legacyCredentialEndpointProofMaxBytes)
	if err != nil {
		return err
	}
	actual, err := decodeCredentialEndpointMaintenanceJournal(read)
	if err != nil || actual != journal {
		return errors.Join(ErrCredentialEndpointMaintenanceTicketMismatch, err)
	}
	return namespace.validateDirectory()
}

func (namespace *legacyCredentialMaintenanceNamespace) readJournal() (credentialEndpointMaintenanceJournal, error) {
	read, _, err := fsutil.ReadSecureFileInDirectoryWithIdentity(namespace.fs, namespace.directory, namespace.journalName, legacyCredentialEndpointProofMaxBytes)
	if err != nil {
		return credentialEndpointMaintenanceJournal{}, errors.Join(ErrCredentialEndpointMaintenancePending, err)
	}
	journal, err := decodeCredentialEndpointMaintenanceJournal(read)
	if err != nil {
		return credentialEndpointMaintenanceJournal{}, errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
	}
	return journal, nil
}

func (namespace *legacyCredentialMaintenanceNamespace) readExpectedJournal(ticket LegacyCredentialEndpointTransitionTicket) (credentialEndpointMaintenanceJournal, error) {
	journal, err := namespace.readJournal()
	if err != nil {
		return credentialEndpointMaintenanceJournal{}, err
	}
	if journal.Ticket != ticket {
		return credentialEndpointMaintenanceJournal{}, ErrCredentialEndpointMaintenanceTicketMismatch
	}
	if err := namespace.validateHeldLock(ticket.Lock); err != nil {
		return credentialEndpointMaintenanceJournal{}, err
	}
	if err := namespace.requireEntryAbsent(filepath.Base(credentialEndpointSidecarPath(namespace.path))); err != nil {
		return credentialEndpointMaintenanceJournal{}, errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
	}
	return journal, nil
}

func (namespace *legacyCredentialMaintenanceNamespace) validateJournal(expected credentialEndpointMaintenanceJournal) error {
	actual, err := namespace.readJournal()
	if err != nil || actual != expected {
		return errors.Join(ErrCredentialEndpointMaintenanceTicketMismatch, err)
	}
	return nil
}

func (namespace *legacyCredentialMaintenanceNamespace) validateQuarantinedWithJournal(ticket LegacyCredentialEndpointTransitionTicket, journal credentialEndpointMaintenanceJournal) error {
	if err := namespace.validateJournal(journal); err != nil {
		return err
	}
	return namespace.validateQuarantined(ticket)
}

func (namespace *legacyCredentialMaintenanceNamespace) removeQuarantineForCommit(ticket LegacyCredentialEndpointTransitionTicket, journal credentialEndpointMaintenanceJournal) error {
	if err := namespace.validateJournal(journal); err != nil {
		return err
	}
	if err := namespace.validateHeldLock(ticket.Lock); err != nil {
		return err
	}
	if err := namespace.requireEntryAbsent(namespace.finalName); err != nil {
		return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
	}
	if err := namespace.requireEntryAbsent(filepath.Base(credentialEndpointSidecarPath(namespace.path))); err != nil {
		return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
	}
	quarantine, err := namespace.inspectSocket(ticket.QuarantineName, 1)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil || quarantine != ticket.Socket {
		return errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	if err := unix.Unlinkat(namespace.directoryFD, ticket.QuarantineName, 0); err != nil {
		return err
	}
	if err := namespace.directory.Sync(); err != nil {
		return err
	}
	if err := namespace.requireEntryAbsent(ticket.QuarantineName); err != nil {
		return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
	}
	return namespace.validateJournal(journal)
}

func (namespace *legacyCredentialMaintenanceNamespace) removeJournalForCommit(ticket LegacyCredentialEndpointTransitionTicket, journal credentialEndpointMaintenanceJournal) error {
	data, readIdentity, err := fsutil.ReadSecureFileInDirectoryWithIdentity(namespace.fs, namespace.directory, namespace.journalName, legacyCredentialEndpointProofMaxBytes)
	if err != nil {
		return err
	}
	actual, err := decodeCredentialEndpointMaintenanceJournal(data)
	if err != nil || actual != journal {
		return errors.Join(ErrCredentialEndpointMaintenanceTicketMismatch, err)
	}
	journalProof, err := namespace.inspectRegular(namespace.journalName)
	if err != nil || journalProof.Device != readIdentity.Device || journalProof.Inode != readIdentity.Inode || journalProof.Links != readIdentity.Links {
		return errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	if err := namespace.validateHeldLock(ticket.Lock); err != nil {
		return err
	}
	for _, name := range []string{namespace.finalName, ticket.QuarantineName, filepath.Base(credentialEndpointSidecarPath(namespace.path))} {
		if err := namespace.requireEntryAbsent(name); err != nil {
			return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
		}
	}
	currentJournalProof, err := namespace.inspectRegular(namespace.journalName)
	if err != nil || currentJournalProof != journalProof {
		return errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	if err := unix.Unlinkat(namespace.directoryFD, namespace.journalName, 0); err != nil {
		return err
	}
	if err := namespace.directory.Sync(); err != nil {
		return err
	}
	if err := namespace.requireEntryAbsent(namespace.journalName); err != nil {
		return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
	}
	return namespace.validateHeldLock(ticket.Lock)
}

func (namespace *legacyCredentialMaintenanceNamespace) restoreSocketForRollback(ctx context.Context, ticket LegacyCredentialEndpointTransitionTicket, journal credentialEndpointMaintenanceJournal, authority DrainAuthority) error {
	if err := namespace.validateJournal(journal); err != nil {
		return err
	}
	if err := namespace.validateHeldLock(ticket.Lock); err != nil {
		return err
	}
	if err := namespace.requireEntryAbsent(filepath.Base(credentialEndpointSidecarPath(namespace.path))); err != nil {
		return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
	}
	final, finalExists, err := namespace.inspectOptionalSocket(namespace.finalName)
	if err != nil {
		return err
	}
	quarantine, quarantineExists, err := namespace.inspectOptionalSocket(ticket.QuarantineName)
	if err != nil {
		return err
	}
	switch {
	case !finalExists && quarantineExists:
		if quarantine != ticket.Socket {
			return ErrCredentialEndpointIdentityChanged
		}
		if err := assertLegacyCredentialDrainAuthority(ctx, ticket.Path, authority); err != nil {
			return err
		}
		if err := namespace.validateJournal(journal); err != nil {
			return err
		}
		current, err := namespace.inspectSocket(ticket.QuarantineName, 1)
		if err != nil || current != ticket.Socket {
			return errors.Join(ErrCredentialEndpointIdentityChanged, err)
		}
		if err := namespace.requireEntryAbsent(namespace.finalName); err != nil {
			return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
		}
		if err := unix.Linkat(namespace.directoryFD, ticket.QuarantineName, namespace.directoryFD, namespace.finalName, 0); err != nil {
			return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
		}
		if err := namespace.validateDualSocket(ticket); err != nil {
			return err
		}
		if err := namespace.directory.Sync(); err != nil {
			return err
		}
		if err := namespace.validateDualSocket(ticket); err != nil {
			return err
		}
		final, finalExists, err = namespace.inspectOptionalSocket(namespace.finalName)
		if err != nil {
			return err
		}
		quarantine, quarantineExists, err = namespace.inspectOptionalSocket(ticket.QuarantineName)
		if err != nil {
			return err
		}
	case finalExists && !quarantineExists:
		if final != ticket.Socket {
			return ErrCredentialEndpointIdentityChanged
		}
		return namespace.validateRolledBack(ticket)
	}
	wantLinked := ticket.Socket
	wantLinked.Links = 2
	if !finalExists || !quarantineExists || final != wantLinked || quarantine != wantLinked {
		return ErrCredentialEndpointMaintenanceConflict
	}
	if err := assertLegacyCredentialDrainAuthority(ctx, ticket.Path, authority); err != nil {
		return err
	}
	if err := namespace.validateJournal(journal); err != nil {
		return err
	}
	if err := namespace.validateDualSocket(ticket); err != nil {
		return err
	}
	if err := unix.Unlinkat(namespace.directoryFD, ticket.QuarantineName, 0); err != nil {
		return err
	}
	if err := namespace.directory.Sync(); err != nil {
		return err
	}
	return namespace.validateRolledBack(ticket)
}

func (namespace *legacyCredentialMaintenanceNamespace) validateRolledBack(ticket LegacyCredentialEndpointTransitionTicket) error {
	if err := namespace.validateDirectory(); err != nil {
		return err
	}
	if err := namespace.validateHeldLock(ticket.Lock); err != nil {
		return err
	}
	final, err := namespace.inspectSocket(namespace.finalName, 1)
	if err != nil || final != ticket.Socket {
		return errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	for _, name := range []string{ticket.QuarantineName, filepath.Base(credentialEndpointSidecarPath(namespace.path))} {
		if err := namespace.requireEntryAbsent(name); err != nil {
			return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
		}
	}
	return namespace.validateDirectory()
}

func (namespace *legacyCredentialMaintenanceNamespace) validateRolledBackWithJournal(ticket LegacyCredentialEndpointTransitionTicket, journal credentialEndpointMaintenanceJournal) error {
	if err := namespace.validateJournal(journal); err != nil {
		return err
	}
	return namespace.validateRolledBack(ticket)
}

func (namespace *legacyCredentialMaintenanceNamespace) inspectOptionalSocket(name string) (LegacyCredentialEndpointIdentity, bool, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(namespace.directoryFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
		return LegacyCredentialEndpointIdentity{}, false, nil
	} else if err != nil {
		return LegacyCredentialEndpointIdentity{}, false, err
	}
	proof := LegacyCredentialEndpointIdentity{
		Device: uint64(stat.Dev), Inode: uint64(stat.Ino), UID: uint64(stat.Uid), Links: uint64(stat.Nlink),
		Type: "socket", Mode: uint32(stat.Mode) & 0o777,
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK || stat.Mode&(unix.S_ISUID|unix.S_ISGID|unix.S_ISVTX) != 0 || proof.UID != uint64(os.Geteuid()) || proof.Mode != 0o600 || (proof.Links != 1 && proof.Links != 2) {
		return LegacyCredentialEndpointIdentity{}, true, ErrCredentialEndpointIdentityChanged
	}
	return proof, true, nil
}

func (namespace *legacyCredentialMaintenanceNamespace) validateReadOnlyJournalShape(journal credentialEndpointMaintenanceJournal) error {
	ticket := journal.Ticket
	if err := namespace.requireEntryAbsent(filepath.Base(credentialEndpointSidecarPath(namespace.path))); err != nil {
		return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
	}
	final, finalExists, err := namespace.inspectOptionalSocket(namespace.finalName)
	if err != nil {
		return err
	}
	quarantine, quarantineExists, err := namespace.inspectOptionalSocket(ticket.QuarantineName)
	if err != nil {
		return err
	}
	wantLinked := ticket.Socket
	wantLinked.Links = 2
	finalOnly := finalExists && !quarantineExists && final == ticket.Socket
	quarantineOnly := !finalExists && quarantineExists && quarantine == ticket.Socket
	dual := finalExists && quarantineExists && final == wantLinked && quarantine == wantLinked
	neither := !finalExists && !quarantineExists
	valid := false
	switch journal.State {
	case CredentialEndpointMaintenancePrepared:
		valid = finalOnly || quarantineOnly || dual
	case CredentialEndpointMaintenanceQuarantined:
		valid = quarantineOnly
	case CredentialEndpointMaintenanceCommitting:
		valid = quarantineOnly || neither
	case CredentialEndpointMaintenanceRollingBack:
		valid = quarantineOnly || dual || finalOnly
	case CredentialEndpointMaintenanceRolledBack:
		valid = finalOnly
	}
	if !valid {
		return ErrCredentialEndpointMaintenanceConflict
	}
	return namespace.validateDirectory()
}

func (namespace *legacyCredentialMaintenanceNamespace) detachSocket(ctx context.Context, ticket LegacyCredentialEndpointTransitionTicket, journal credentialEndpointMaintenanceJournal, authority DrainAuthority) error {
	if err := namespace.validateJournal(journal); err != nil {
		return err
	}
	if err := namespace.validateHeldLock(ticket.Lock); err != nil {
		return err
	}
	if err := namespace.requireEntryAbsent(filepath.Base(credentialEndpointSidecarPath(namespace.path))); err != nil {
		return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
	}
	final, finalExists, err := namespace.inspectOptionalSocket(namespace.finalName)
	if err != nil {
		return err
	}
	quarantine, quarantineExists, err := namespace.inspectOptionalSocket(ticket.QuarantineName)
	if err != nil {
		return err
	}
	switch {
	case !finalExists && quarantineExists:
		if quarantine != ticket.Socket {
			return ErrCredentialEndpointIdentityChanged
		}
		return namespace.validateQuarantined(ticket)
	case finalExists && !quarantineExists:
		if final != ticket.Socket {
			return ErrCredentialEndpointIdentityChanged
		}
		if err := assertLegacyCredentialDrainAuthority(ctx, ticket.Path, authority); err != nil {
			return err
		}
		if err := namespace.validateJournal(journal); err != nil {
			return err
		}
		current, err := namespace.inspectSocket(namespace.finalName, 1)
		if err != nil || current != ticket.Socket {
			return errors.Join(ErrCredentialEndpointIdentityChanged, err)
		}
		if err := namespace.requireEntryAbsent(ticket.QuarantineName); err != nil {
			return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
		}
		if err := unix.Linkat(namespace.directoryFD, namespace.finalName, namespace.directoryFD, ticket.QuarantineName, 0); err != nil {
			return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
		}
		if err := namespace.validateDualSocket(ticket); err != nil {
			return err
		}
		if err := namespace.directory.Sync(); err != nil {
			return err
		}
		if err := namespace.validateDualSocket(ticket); err != nil {
			return err
		}
		final, finalExists, err = namespace.inspectOptionalSocket(namespace.finalName)
		if err != nil {
			return err
		}
		quarantine, quarantineExists, err = namespace.inspectOptionalSocket(ticket.QuarantineName)
		if err != nil {
			return err
		}
	}
	wantLinked := ticket.Socket
	wantLinked.Links = 2
	if !finalExists || !quarantineExists || final != wantLinked || quarantine != wantLinked {
		return ErrCredentialEndpointMaintenanceConflict
	}
	if err := assertLegacyCredentialDrainAuthority(ctx, ticket.Path, authority); err != nil {
		return err
	}
	if err := namespace.validateJournal(journal); err != nil {
		return err
	}
	currentFinal, err := namespace.inspectSocket(namespace.finalName, 2)
	if err != nil || currentFinal != wantLinked {
		return errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	currentQuarantine, err := namespace.inspectSocket(ticket.QuarantineName, 2)
	if err != nil || currentQuarantine != wantLinked {
		return errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	if err := unix.Unlinkat(namespace.directoryFD, namespace.finalName, 0); err != nil {
		return err
	}
	if err := namespace.directory.Sync(); err != nil {
		return err
	}
	if err := namespace.validateJournal(journal); err != nil {
		return err
	}
	return namespace.validateQuarantined(ticket)
}

func (namespace *legacyCredentialMaintenanceNamespace) validateQuarantined(ticket LegacyCredentialEndpointTransitionTicket) error {
	if err := namespace.validateDirectory(); err != nil {
		return err
	}
	if err := namespace.validateHeldLock(ticket.Lock); err != nil {
		return err
	}
	if err := namespace.requireEntryAbsent(namespace.finalName); err != nil {
		return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
	}
	quarantine, err := namespace.inspectSocket(ticket.QuarantineName, 1)
	if err != nil || quarantine != ticket.Socket {
		return errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	return namespace.validateDirectory()
}

func (namespace *legacyCredentialMaintenanceNamespace) validateDualSocket(ticket LegacyCredentialEndpointTransitionTicket) error {
	want := ticket.Socket
	want.Links = 2
	final, finalErr := namespace.inspectSocket(namespace.finalName, 2)
	quarantine, quarantineErr := namespace.inspectSocket(ticket.QuarantineName, 2)
	if finalErr != nil || quarantineErr != nil || final != want || quarantine != want {
		return errors.Join(ErrCredentialEndpointIdentityChanged, finalErr, quarantineErr)
	}
	return nil
}

func newLegacyCredentialEndpointTicketID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}
