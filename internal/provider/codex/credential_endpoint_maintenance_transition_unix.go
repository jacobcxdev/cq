//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package codex

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
	rollbackName   string
}

type legacyCredentialEndpointTransition struct {
	mu           sync.Mutex
	namespace    *legacyCredentialMaintenanceNamespace
	lock         fsutil.ExclusiveLock
	ticket       LegacyCredentialEndpointTransitionTicket
	journal      credentialEndpointMaintenanceJournal
	recordName   string
	authority    DrainAuthority
	rollbackHook credentialEndpointPhaseHook
	closed       bool
}

type legacyCredentialMaintenanceRecordPair struct {
	journal        credentialEndpointMaintenanceJournal
	journalExists  bool
	rollback       credentialEndpointMaintenanceJournal
	rollbackExists bool
}

func (pair legacyCredentialMaintenanceRecordPair) equal(other legacyCredentialMaintenanceRecordPair) bool {
	return pair.journalExists == other.journalExists && pair.rollbackExists == other.rollbackExists &&
		(!pair.journalExists || credentialEndpointMaintenanceJournalsEqual(pair.journal, other.journal)) &&
		(!pair.rollbackExists || credentialEndpointMaintenanceJournalsEqual(pair.rollback, other.rollback))
}

func (pair legacyCredentialMaintenanceRecordPair) ticket() (LegacyCredentialEndpointTransitionTicket, error) {
	switch {
	case pair.journalExists:
		return pair.journal.Ticket, nil
	case pair.rollbackExists:
		return pair.rollback.Ticket, nil
	default:
		return LegacyCredentialEndpointTransitionTicket{}, ErrCredentialEndpointMaintenancePending
	}
}

func (pair legacyCredentialMaintenanceRecordPair) statusRecord() (credentialEndpointMaintenanceJournal, error) {
	switch {
	case pair.journalExists:
		return pair.journal, nil
	case pair.rollbackExists:
		return pair.rollback, nil
	default:
		return credentialEndpointMaintenanceJournal{}, ErrCredentialEndpointMaintenancePending
	}
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
	transition := &legacyCredentialEndpointTransition{
		namespace: namespace, lock: lock, authority: authority, recordName: namespace.journalName,
	}
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
		lockName:     filepath.Base(credentialEndpointLockPath(path)),
		journalName:  filepath.Base(credentialEndpointMaintenanceJournalPath(path)),
		rollbackName: filepath.Base(credentialEndpointMaintenanceRollbackPath(path)),
	}
	defer namespace.Close()
	proof, err := inspectLegacyCredentialDirectory(fsys, directory, directoryFD, directoryPath)
	if err != nil {
		return LegacyCredentialEndpointTransitionStatus{}, err
	}
	namespace.directoryProof = proof
	pair, err := namespace.readMaintenanceRecordPair()
	if err != nil {
		return LegacyCredentialEndpointTransitionStatus{}, err
	}
	record, err := pair.statusRecord()
	if err != nil {
		return LegacyCredentialEndpointTransitionStatus{}, err
	}
	if record.Ticket.Path != path || legacyCredentialEndpointDirectoryDifference(proof, record.Ticket.Directory) != "" {
		return LegacyCredentialEndpointTransitionStatus{}, ErrCredentialEndpointMaintenanceTicketMismatch
	}
	lock, err := namespace.inspectRegular(namespace.lockName)
	if err != nil || lock != record.Ticket.Lock {
		return LegacyCredentialEndpointTransitionStatus{}, errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
	}
	if err := namespace.validateReadOnlyRecordPairShape(pair); err != nil {
		return LegacyCredentialEndpointTransitionStatus{}, err
	}
	return LegacyCredentialEndpointTransitionStatus{State: record.State, Ticket: record.Ticket}, nil
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
	pair, err := namespace.readMaintenanceRecordPair()
	pairTicket, pairTicketErr := pair.ticket()
	if err != nil || pairTicketErr != nil || pairTicket != ticket {
		return nil, errors.Join(ErrCredentialEndpointMaintenanceTicketMismatch, err, pairTicketErr)
	}
	lockProof, err := namespace.inspectRegular(namespace.lockName)
	if err != nil || lockProof != ticket.Lock {
		return nil, errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
	}
	lock, err := namespace.openExistingLock(ticket.Lock)
	if err != nil {
		return nil, err
	}
	record, err := pair.statusRecord()
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	recordName := namespace.journalName
	if !pair.journalExists {
		recordName = namespace.rollbackName
	}
	transition := &legacyCredentialEndpointTransition{
		namespace: namespace, lock: lock, ticket: ticket, journal: record, recordName: recordName, authority: authority,
	}
	closeNamespace = false
	closeTransition := true
	defer func() {
		if closeTransition {
			_ = transition.Close()
		}
	}()
	currentPair, err := namespace.readMaintenanceRecordPair()
	if err != nil || !currentPair.equal(pair) {
		return nil, errors.Join(ErrCredentialEndpointMaintenanceTicketMismatch, err)
	}
	if err := namespace.validateHeldLock(ticket.Lock); err != nil {
		return nil, err
	}
	if err := assertLegacyCredentialDrainAuthority(ctx, path, authority); err != nil {
		return nil, err
	}
	public := &LegacyCredentialEndpointTransition{implementation: transition}
	if pair.journalExists && pair.rollbackExists &&
		pair.journal.State == CredentialEndpointMaintenanceRolledBack &&
		pair.rollback.State == CredentialEndpointMaintenanceRollingBack {
		if err := transition.finishActivatedRollbackReceipt(ctx, pair.journal, pair.rollback); err != nil {
			return nil, err
		}
		closeTransition = false
		return public, nil
	}
	switch record.State {
	case CredentialEndpointMaintenancePrepared:
		if err := transition.resumePrepared(ctx); err != nil {
			return nil, err
		}
	case CredentialEndpointMaintenanceQuarantined:
		if err := namespace.validateQuarantinedWithJournal(ticket, record); err != nil {
			return nil, err
		}
	case CredentialEndpointMaintenanceActivating:
		if !pair.journalExists {
			return nil, ErrCredentialEndpointMaintenanceConflict
		}
		if err := public.Activate(ctx); err != nil {
			return nil, err
		}
	case CredentialEndpointMaintenanceActivated:
		if pair.journalExists || !pair.rollbackExists {
			return nil, ErrCredentialEndpointMaintenanceConflict
		}
		if err := namespace.validateActivatedRecord(record, record.Owner == nil); err != nil {
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

func (transition *legacyCredentialEndpointTransition) Activate(ctx context.Context) error {
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
	if transition.recordName == transition.namespace.rollbackName {
		record, exists, err := transition.namespace.readOptionalRecord(transition.namespace.rollbackName)
		if err != nil || !exists || record.State != CredentialEndpointMaintenanceActivated ||
			!credentialEndpointMaintenanceJournalsEqual(record, transition.journal) {
			return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
		}
		if err := transition.namespace.requireEntryAbsent(transition.namespace.journalName); err != nil {
			return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
		}
		return transition.namespace.validateActivatedRecord(record, record.Owner == nil)
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
		if err := transition.namespace.requireEntryAbsent(transition.namespace.rollbackName); err != nil {
			return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
		}
		if err := assertLegacyCredentialDrainAuthority(ctx, transition.ticket.Path, transition.authority); err != nil {
			return err
		}
		activating := journal
		activating.Generation++
		activating.State = CredentialEndpointMaintenanceActivating
		if err := transition.namespace.writeJournal(activating, func() error {
			if err := transition.namespace.validateJournal(journal); err != nil {
				return err
			}
			if err := transition.namespace.requireEntryAbsent(transition.namespace.rollbackName); err != nil {
				return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
			}
			return transition.namespace.validateQuarantined(transition.ticket)
		}); err != nil {
			return fmt.Errorf("write activating maintenance journal: %w", err)
		}
		journal = activating
		transition.journal = activating
	case CredentialEndpointMaintenanceActivating:
		transition.journal = journal
	default:
		return fmt.Errorf("%w: cannot activate state %q", ErrCredentialEndpointMaintenanceConflict, journal.State)
	}

	record, exists, err := transition.namespace.readOptionalRecord(transition.namespace.rollbackName)
	if err != nil {
		return err
	}
	if !exists {
		if err := assertLegacyCredentialDrainAuthority(ctx, transition.ticket.Path, transition.authority); err != nil {
			return err
		}
		record = journal
		record.Generation++
		record.State = CredentialEndpointMaintenanceActivated
		record.Owner = nil
		if err := transition.namespace.writeRecord(transition.namespace.rollbackName, record, func() error {
			if err := transition.namespace.validateJournal(journal); err != nil {
				return err
			}
			if err := transition.namespace.requireEntryAbsent(transition.namespace.rollbackName); err != nil {
				return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
			}
			return transition.namespace.validateQuarantined(transition.ticket)
		}); err != nil {
			return fmt.Errorf("write activated rollback record: %w", err)
		}
	} else if record.State != CredentialEndpointMaintenanceActivated || record.Owner != nil ||
		record.Ticket != transition.ticket || record.Generation != journal.Generation+1 {
		return ErrCredentialEndpointMaintenanceConflict
	}
	if err := assertLegacyCredentialDrainAuthority(ctx, transition.ticket.Path, transition.authority); err != nil {
		return err
	}
	if err := transition.namespace.removeRecord(transition.namespace.journalName, journal, func() error {
		if err := transition.namespace.validateRecord(transition.namespace.rollbackName, record); err != nil {
			return err
		}
		return transition.namespace.validateQuarantined(transition.ticket)
	}); err != nil {
		return fmt.Errorf("remove activating maintenance journal: %w", err)
	}
	transition.journal = record
	transition.recordName = transition.namespace.rollbackName
	return nil
}

func (transition *legacyCredentialEndpointTransition) Commit(context.Context) error {
	// Irreversible deletion is authorised only by Finalise after a candidate
	// has published, bound its owner proof, and passed the in-owner verifier.
	// Keeping this legacy method as a typed zero-write failure preserves source
	// compatibility without preserving its unsafe semantics.
	return ErrCredentialEndpointMaintenanceCommitDeprecated
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
	if transition.recordName == transition.namespace.rollbackName {
		return transition.rollbackActivated(ctx)
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

func (transition *legacyCredentialEndpointTransition) rollbackActivated(ctx context.Context) error {
	record, exists, err := transition.namespace.readOptionalRecord(transition.namespace.rollbackName)
	if err != nil || !exists || record.Ticket != transition.ticket {
		return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
	}
	if err := transition.namespace.requireEntryAbsent(transition.namespace.journalName); err != nil {
		return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
	}
	switch record.State {
	case CredentialEndpointMaintenanceActivated:
		if record.Owner == nil {
			for _, name := range []string{transition.namespace.finalName, filepath.Base(credentialEndpointSidecarPath(transition.ticket.Path))} {
				if err := transition.namespace.requireEntryAbsent(name); err != nil {
					return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
				}
			}
		} else {
			if err := transition.cleanupActivatedCandidateForRollback(ctx, record); err != nil {
				return err
			}
		}
		if err := assertLegacyCredentialDrainAuthority(ctx, transition.ticket.Path, transition.authority); err != nil {
			return err
		}
		if err := transition.namespace.validateRecord(transition.namespace.rollbackName, record); err != nil {
			return err
		}
		if err := transition.namespace.validateHeldLock(transition.ticket.Lock); err != nil {
			return err
		}
		quarantine, err := transition.namespace.inspectSocket(transition.ticket.QuarantineName, 1)
		if err != nil || quarantine != transition.ticket.Socket {
			return errors.Join(ErrCredentialEndpointIdentityChanged, err)
		}
		rollingBack := record
		rollingBack.Generation++
		rollingBack.State = CredentialEndpointMaintenanceRollingBack
		if err := transition.namespace.writeRecord(transition.namespace.rollbackName, rollingBack, func() error {
			if err := transition.namespace.requireEntryAbsent(transition.namespace.journalName); err != nil {
				return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
			}
			if err := transition.namespace.validateRecord(transition.namespace.rollbackName, record); err != nil {
				return err
			}
			if err := transition.namespace.validateHeldLock(transition.ticket.Lock); err != nil {
				return err
			}
			for _, name := range []string{transition.namespace.finalName, filepath.Base(credentialEndpointSidecarPath(transition.ticket.Path))} {
				if err := transition.namespace.requireEntryAbsent(name); err != nil {
					return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
				}
			}
			return nil
		}); err != nil {
			return fmt.Errorf("write activated rolling-back receipt: %w", err)
		}
		record = rollingBack
		transition.journal = rollingBack
	case CredentialEndpointMaintenanceRollingBack:
		transition.journal = record
	case CredentialEndpointMaintenanceFinalising:
		return ErrCredentialEndpointMaintenanceConflict
	default:
		return ErrCredentialEndpointMaintenanceConflict
	}

	if err := transition.namespace.restoreSocketForActivatedRollback(ctx, transition.ticket, record, transition.authority); err != nil {
		return err
	}
	if err := assertLegacyCredentialDrainAuthority(ctx, transition.ticket.Path, transition.authority); err != nil {
		return err
	}
	rolledBack := record
	rolledBack.Generation++
	rolledBack.State = CredentialEndpointMaintenanceRolledBack
	rolledBack.Owner = nil
	if err := transition.namespace.writeJournal(rolledBack, func() error {
		if err := transition.namespace.requireEntryAbsent(transition.namespace.journalName); err != nil {
			return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
		}
		if err := transition.namespace.validateRecord(transition.namespace.rollbackName, record); err != nil {
			return err
		}
		return transition.namespace.validateRolledBack(transition.ticket)
	}); err != nil {
		return fmt.Errorf("write activated rolled-back journal: %w", err)
	}
	if err := transition.finishActivatedRollbackReceipt(ctx, rolledBack, record); err != nil {
		return err
	}
	return nil
}

func (transition *legacyCredentialEndpointTransition) cleanupActivatedCandidateForRollback(ctx context.Context, record credentialEndpointMaintenanceJournal) error {
	if record.Owner == nil {
		return ErrCredentialEndpointMaintenanceConflict
	}
	namespace := transition.namespace
	finalLegacy, finalExists, err := namespace.inspectOptionalSocket(namespace.finalName)
	if err != nil {
		return err
	}
	sidecarName := filepath.Base(credentialEndpointSidecarPath(transition.ticket.Path))
	data, readIdentity, err := fsutil.ReadSecureFileInDirectoryWithIdentity(
		namespace.fs, namespace.directory, sidecarName, credentialEndpointSidecarMaxBytes,
	)
	if errors.Is(err, os.ErrNotExist) && !finalExists {
		return nil
	}
	if err != nil {
		return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
	}
	sidecarProof, err := namespace.inspectRegular(sidecarName)
	if err != nil || sidecarProof.Device != readIdentity.Device || sidecarProof.Inode != readIdentity.Inode || sidecarProof.Links != readIdentity.Links {
		return errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	sidecar, err := decodeCredentialEndpointSidecar(data, transition.ticket.Path)
	if err != nil || sidecar.Generation != record.Owner.Generation || sidecar.Previous != nil ||
		!maintenanceSidecarMatchesOwnerSocket(sidecar, record.Owner.Socket) {
		return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
	}
	if finalExists && finalLegacy != record.Owner.Socket {
		return ErrCredentialEndpointIdentityChanged
	}
	switch sidecar.State {
	case credentialEndpointPublished:
		if !finalExists || sidecarProof != record.Owner.SidecarFile || digestMaintenanceBytes(data) != record.Owner.SidecarSHA256 {
			return ErrCredentialEndpointIdentityChanged
		}
	case credentialEndpointClosing:
		published := sidecar
		published.State = credentialEndpointPublished
		publishedData, marshalErr := json.Marshal(published)
		if marshalErr != nil || digestMaintenanceBytes(publishedData) != record.Owner.SidecarSHA256 {
			return errors.Join(ErrCredentialEndpointIdentityChanged, marshalErr)
		}
	default:
		return ErrCredentialEndpointMaintenanceConflict
	}
	if finalExists {
		client, protocol, _, probeErr := probeCredentialOwnerAttempt(
			transition.ticket.Path, credentialEndpointDialTimeout, net.DialTimeout,
		)
		if client != nil {
			_ = client.Close()
			return ErrCredentialEndpointLockHeld
		}
		if protocol != credentialOwnerRefused {
			return errors.Join(ErrCredentialEndpointMaintenanceConflict, probeErr)
		}
	}
	if err := assertLegacyCredentialDrainAuthority(ctx, transition.ticket.Path, transition.authority); err != nil {
		return err
	}
	if err := namespace.validateRecord(namespace.rollbackName, record); err != nil {
		return err
	}
	if err := namespace.validateHeldLock(transition.ticket.Lock); err != nil {
		return err
	}
	quarantine, err := namespace.inspectSocket(transition.ticket.QuarantineName, 1)
	if err != nil || quarantine != transition.ticket.Socket {
		return errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	directoryInfo, err := namespace.directory.Stat()
	if err != nil {
		return err
	}
	directoryID, ok := namespace.fs.FileIdentity(directoryInfo)
	if !ok {
		return fsutil.ErrUnsafeSecurePath
	}
	candidate := &credentialEndpoint{
		fs: namespace.fs, secureDirectory: namespace.directory, directoryFD: namespace.directoryFD,
		directory: namespace.directoryPath, directoryID: directoryID,
		path: transition.ticket.Path, finalName: namespace.finalName,
		generation: sidecar.Generation, identity: sidecar.credentialEndpointIdentity,
		hook:       transition.rollbackHook,
		sidecarCAS: &credentialEndpointSidecarCAS{identity: readIdentity, digest: digestMaintenanceBytes(data)},
		lockIdentity: fsutil.SecureFileIdentity{
			Device: transition.ticket.Lock.Device, Inode: transition.ticket.Lock.Inode, Links: transition.ticket.Lock.Links,
		},
		sidecar: sidecar,
	}
	finalIdentity, currentFinalExists, err := statCredentialEndpointSocketAt(namespace.directoryFD, namespace.finalName, true)
	if err != nil || currentFinalExists != finalExists || (finalExists && finalIdentity != sidecar.credentialEndpointIdentity) {
		return errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	candidate.invokePhase(credentialEndpointPhaseMaintenanceRollbackCandidateValidated)
	if sidecar.State == credentialEndpointPublished {
		if err := candidate.closePublished(); err != nil {
			return err
		}
	} else if err := candidate.cleanInterruptedPublication(sidecar, finalIdentity, finalExists); err != nil {
		return err
	}
	for _, name := range []string{namespace.finalName, sidecarName} {
		if err := namespace.requireEntryAbsent(name); err != nil {
			return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
		}
	}
	return namespace.validateRecord(namespace.rollbackName, record)
}

func (transition *legacyCredentialEndpointTransition) finishActivatedRollbackReceipt(ctx context.Context, journal, record credentialEndpointMaintenanceJournal) error {
	if journal.State != CredentialEndpointMaintenanceRolledBack || journal.Owner != nil ||
		record.State != CredentialEndpointMaintenanceRollingBack || journal.Ticket != transition.ticket ||
		record.Ticket != transition.ticket || journal.Generation != record.Generation+1 {
		return ErrCredentialEndpointMaintenanceConflict
	}
	if err := transition.namespace.validateJournal(journal); err != nil {
		return err
	}
	if err := transition.namespace.validateRecord(transition.namespace.rollbackName, record); err != nil {
		return err
	}
	if err := transition.namespace.validateRolledBack(transition.ticket); err != nil {
		return err
	}
	if err := assertLegacyCredentialDrainAuthority(ctx, transition.ticket.Path, transition.authority); err != nil {
		return err
	}
	if err := transition.namespace.removeRecord(transition.namespace.rollbackName, record, func() error {
		if err := transition.namespace.validateJournal(journal); err != nil {
			return err
		}
		return transition.namespace.validateRolledBack(transition.ticket)
	}); err != nil {
		return fmt.Errorf("remove activated rollback receipt: %w", err)
	}
	transition.journal = journal
	transition.recordName = transition.namespace.journalName
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
		lockName:     filepath.Base(credentialEndpointLockPath(path)),
		journalName:  filepath.Base(credentialEndpointMaintenanceJournalPath(path)),
		rollbackName: filepath.Base(credentialEndpointMaintenanceRollbackPath(path)),
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
	if err := namespace.requireEntryAbsent(filepath.Base(credentialEndpointMaintenanceRollbackPath(namespace.path))); err != nil {
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
	return namespace.writeRecord(namespace.journalName, journal, precondition)
}

func (namespace *legacyCredentialMaintenanceNamespace) writeRecord(name string, journal credentialEndpointMaintenanceJournal, precondition func() error) error {
	data, err := encodeCredentialEndpointMaintenanceJournal(journal)
	if err != nil {
		return err
	}
	if err := fsutil.SecureAtomicWriteInDirectoryChecked(namespace.fs, namespace.directory, name, data, func() error {
		if err := namespace.validateDirectory(); err != nil {
			return err
		}
		return precondition()
	}); err != nil {
		return err
	}
	read, _, err := fsutil.ReadSecureFileInDirectoryWithIdentity(namespace.fs, namespace.directory, name, legacyCredentialEndpointProofMaxBytes)
	if err != nil {
		return err
	}
	actual, err := decodeCredentialEndpointMaintenanceJournal(read)
	if err != nil || !credentialEndpointMaintenanceJournalsEqual(actual, journal) {
		return errors.Join(ErrCredentialEndpointMaintenanceTicketMismatch, err)
	}
	return namespace.validateDirectory()
}

func (namespace *legacyCredentialMaintenanceNamespace) readOptionalRecord(name string) (credentialEndpointMaintenanceJournal, bool, error) {
	read, _, err := fsutil.ReadSecureFileInDirectoryWithIdentity(namespace.fs, namespace.directory, name, legacyCredentialEndpointProofMaxBytes)
	if errors.Is(err, os.ErrNotExist) {
		return credentialEndpointMaintenanceJournal{}, false, nil
	}
	if err != nil {
		return credentialEndpointMaintenanceJournal{}, true, errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
	}
	record, err := decodeCredentialEndpointMaintenanceJournal(read)
	if err != nil {
		return credentialEndpointMaintenanceJournal{}, true, errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
	}
	return record, true, nil
}

func (namespace *legacyCredentialMaintenanceNamespace) readMaintenanceRecordPair() (legacyCredentialMaintenanceRecordPair, error) {
	journal, journalExists, journalErr := namespace.readOptionalRecord(namespace.journalName)
	rollback, rollbackExists, rollbackErr := namespace.readOptionalRecord(namespace.rollbackName)
	if journalErr != nil || rollbackErr != nil {
		return legacyCredentialMaintenanceRecordPair{}, errors.Join(ErrCredentialEndpointMaintenancePending, journalErr, rollbackErr)
	}
	pair := legacyCredentialMaintenanceRecordPair{
		journal: journal, journalExists: journalExists,
		rollback: rollback, rollbackExists: rollbackExists,
	}
	if !journalExists && !rollbackExists {
		return pair, ErrCredentialEndpointMaintenancePending
	}
	if journalExists {
		if journal.Owner != nil {
			return pair, ErrCredentialEndpointMaintenanceConflict
		}
		switch journal.State {
		case CredentialEndpointMaintenancePrepared,
			CredentialEndpointMaintenanceQuarantined,
			CredentialEndpointMaintenanceActivating,
			CredentialEndpointMaintenanceCommitting,
			CredentialEndpointMaintenanceRollingBack,
			CredentialEndpointMaintenanceRolledBack:
		default:
			return pair, ErrCredentialEndpointMaintenanceConflict
		}
	}
	if rollbackExists {
		switch rollback.State {
		case CredentialEndpointMaintenanceActivated,
			CredentialEndpointMaintenanceFinalising,
			CredentialEndpointMaintenanceRollingBack:
		default:
			return pair, ErrCredentialEndpointMaintenanceConflict
		}
	}
	if journalExists && rollbackExists {
		if journal.Ticket != rollback.Ticket || journal.TicketHash != rollback.TicketHash {
			return pair, ErrCredentialEndpointMaintenanceTicketMismatch
		}
		activationPair := journal.State == CredentialEndpointMaintenanceActivating &&
			rollback.State == CredentialEndpointMaintenanceActivated && rollback.Owner == nil &&
			rollback.Generation == journal.Generation+1
		rollbackPair := journal.State == CredentialEndpointMaintenanceRolledBack &&
			rollback.State == CredentialEndpointMaintenanceRollingBack &&
			journal.Generation == rollback.Generation+1
		validPair := activationPair || rollbackPair
		if !validPair {
			return pair, ErrCredentialEndpointMaintenanceConflict
		}
	}
	return pair, nil
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
	pair, err := namespace.readMaintenanceRecordPair()
	if err != nil {
		return credentialEndpointMaintenanceJournal{}, err
	}
	if !pair.journalExists {
		return credentialEndpointMaintenanceJournal{}, ErrCredentialEndpointMaintenancePending
	}
	journal := pair.journal
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
	if err != nil || !credentialEndpointMaintenanceJournalsEqual(actual, expected) {
		return errors.Join(ErrCredentialEndpointMaintenanceTicketMismatch, err)
	}
	return nil
}

func (namespace *legacyCredentialMaintenanceNamespace) validateRecord(name string, expected credentialEndpointMaintenanceJournal) error {
	actual, exists, err := namespace.readOptionalRecord(name)
	if err != nil || !exists || !credentialEndpointMaintenanceJournalsEqual(actual, expected) {
		return errors.Join(ErrCredentialEndpointMaintenanceTicketMismatch, err)
	}
	return nil
}

func (namespace *legacyCredentialMaintenanceNamespace) removeRecord(name string, expected credentialEndpointMaintenanceJournal, precondition func() error) error {
	data, readIdentity, err := fsutil.ReadSecureFileInDirectoryWithIdentity(namespace.fs, namespace.directory, name, legacyCredentialEndpointProofMaxBytes)
	if err != nil {
		return err
	}
	actual, err := decodeCredentialEndpointMaintenanceJournal(data)
	if err != nil || !credentialEndpointMaintenanceJournalsEqual(actual, expected) {
		return errors.Join(ErrCredentialEndpointMaintenanceTicketMismatch, err)
	}
	proof, err := namespace.inspectRegular(name)
	if err != nil || proof.Device != readIdentity.Device || proof.Inode != readIdentity.Inode || proof.Links != readIdentity.Links {
		return errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	if err := namespace.validateDirectory(); err != nil {
		return err
	}
	if precondition != nil {
		if err := precondition(); err != nil {
			return err
		}
	}
	current, err := namespace.inspectRegular(name)
	if err != nil || current != proof {
		return errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	if err := unix.Unlinkat(namespace.directoryFD, name, 0); err != nil {
		return err
	}
	if err := namespace.directory.Sync(); err != nil {
		return err
	}
	if err := namespace.requireEntryAbsent(name); err != nil {
		return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
	}
	return namespace.validateDirectory()
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
	if err != nil || !credentialEndpointMaintenanceJournalsEqual(actual, journal) {
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

func (namespace *legacyCredentialMaintenanceNamespace) restoreSocketForActivatedRollback(ctx context.Context, ticket LegacyCredentialEndpointTransitionTicket, record credentialEndpointMaintenanceJournal, authority DrainAuthority) error {
	if record.State != CredentialEndpointMaintenanceRollingBack || record.Ticket != ticket {
		return ErrCredentialEndpointMaintenanceConflict
	}
	if err := namespace.validateRecord(namespace.rollbackName, record); err != nil {
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
		if err := namespace.validateRecord(namespace.rollbackName, record); err != nil {
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
	if err := namespace.validateRecord(namespace.rollbackName, record); err != nil {
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
	case CredentialEndpointMaintenanceQuarantined, CredentialEndpointMaintenanceActivating:
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

func (namespace *legacyCredentialMaintenanceNamespace) validateReadOnlyRecordPairShape(pair legacyCredentialMaintenanceRecordPair) error {
	if pair.journalExists && !pair.rollbackExists {
		return namespace.validateReadOnlyJournalShape(pair.journal)
	}
	if pair.journalExists && pair.rollbackExists {
		if pair.journal.State == CredentialEndpointMaintenanceActivating && pair.rollback.State == CredentialEndpointMaintenanceActivated {
			if pair.rollback.Owner != nil {
				return ErrCredentialEndpointMaintenanceConflict
			}
			return namespace.validateActivatedRecord(pair.rollback, true)
		}
		if pair.journal.State == CredentialEndpointMaintenanceRolledBack && pair.rollback.State == CredentialEndpointMaintenanceRollingBack {
			return namespace.validateReadOnlyJournalShape(pair.journal)
		}
		return ErrCredentialEndpointMaintenanceConflict
	}
	if pair.rollbackExists {
		switch pair.rollback.State {
		case CredentialEndpointMaintenanceActivated:
			return namespace.validateActivatedRecord(pair.rollback, pair.rollback.Owner == nil)
		case CredentialEndpointMaintenanceFinalising:
			return namespace.validateFinalisingRecord(pair.rollback)
		case CredentialEndpointMaintenanceRollingBack:
			return namespace.validateActivatedRollbackShape(pair.rollback)
		}
	}
	return ErrCredentialEndpointMaintenanceConflict
}

func (namespace *legacyCredentialMaintenanceNamespace) validateActivatedRecord(record credentialEndpointMaintenanceJournal, requireEmptyCandidate bool) error {
	if record.State != CredentialEndpointMaintenanceActivated || record.Ticket.Path != namespace.path {
		return ErrCredentialEndpointMaintenanceConflict
	}
	if err := namespace.validateDirectory(); err != nil {
		return err
	}
	lock, err := namespace.inspectRegular(namespace.lockName)
	if err != nil || lock != record.Ticket.Lock {
		return errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	quarantine, err := namespace.inspectSocket(record.Ticket.QuarantineName, 1)
	if err != nil || quarantine != record.Ticket.Socket {
		return errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	if requireEmptyCandidate {
		for _, name := range []string{namespace.finalName, filepath.Base(credentialEndpointSidecarPath(namespace.path))} {
			if err := namespace.requireEntryAbsent(name); err != nil {
				return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
			}
		}
	} else if record.Owner != nil {
		if err := namespace.validateActivatedOwnerArtifacts(record); err != nil {
			return err
		}
	}
	return namespace.validateDirectory()
}

func (namespace *legacyCredentialMaintenanceNamespace) validateActivatedOwnerArtifacts(record credentialEndpointMaintenanceJournal) error {
	if record.Owner == nil {
		return ErrCredentialEndpointMaintenanceConflict
	}
	final, finalExists, err := namespace.inspectOptionalSocket(namespace.finalName)
	if err != nil {
		return err
	}
	sidecarName := filepath.Base(credentialEndpointSidecarPath(namespace.path))
	data, readIdentity, err := fsutil.ReadSecureFileInDirectoryWithIdentity(
		namespace.fs, namespace.directory, sidecarName, credentialEndpointSidecarMaxBytes,
	)
	if errors.Is(err, os.ErrNotExist) {
		if finalExists {
			return ErrCredentialEndpointMaintenanceConflict
		}
		return nil
	}
	if err != nil {
		return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
	}
	sidecarProof, err := namespace.inspectRegular(sidecarName)
	if err != nil || sidecarProof.Device != readIdentity.Device || sidecarProof.Inode != readIdentity.Inode || sidecarProof.Links != readIdentity.Links {
		return errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	sidecar, err := decodeCredentialEndpointSidecar(data, namespace.path)
	if err != nil {
		return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
	}
	if sidecar.Generation != record.Owner.Generation || sidecar.Previous != nil ||
		!maintenanceSidecarMatchesOwnerSocket(sidecar, record.Owner.Socket) {
		return ErrCredentialEndpointIdentityChanged
	}
	switch sidecar.State {
	case credentialEndpointPublished:
		if !finalExists || final != record.Owner.Socket || sidecarProof != record.Owner.SidecarFile ||
			digestMaintenanceBytes(data) != record.Owner.SidecarSHA256 {
			return ErrCredentialEndpointIdentityChanged
		}
	case credentialEndpointClosing:
		if finalExists && final != record.Owner.Socket {
			return ErrCredentialEndpointIdentityChanged
		}
		published := sidecar
		published.State = credentialEndpointPublished
		publishedData, marshalErr := json.Marshal(published)
		if marshalErr != nil || digestMaintenanceBytes(publishedData) != record.Owner.SidecarSHA256 {
			return errors.Join(ErrCredentialEndpointIdentityChanged, marshalErr)
		}
	default:
		return ErrCredentialEndpointMaintenanceConflict
	}
	return nil
}

func (namespace *legacyCredentialMaintenanceNamespace) validateFinalisingRecord(record credentialEndpointMaintenanceJournal) error {
	if record.State != CredentialEndpointMaintenanceFinalising || record.Owner == nil {
		return ErrCredentialEndpointMaintenanceConflict
	}
	if err := namespace.validateDirectory(); err != nil {
		return err
	}
	lock, err := namespace.inspectRegular(namespace.lockName)
	if err != nil || lock != record.Ticket.Lock {
		return errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	quarantine, exists, err := namespace.inspectOptionalSocket(record.Ticket.QuarantineName)
	if err != nil || (exists && quarantine != record.Ticket.Socket) {
		return errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	return namespace.validateDirectory()
}

func (namespace *legacyCredentialMaintenanceNamespace) validateActivatedRollbackShape(record credentialEndpointMaintenanceJournal) error {
	if record.State != CredentialEndpointMaintenanceRollingBack || record.Ticket.Path != namespace.path {
		return ErrCredentialEndpointMaintenanceConflict
	}
	if err := namespace.validateDirectory(); err != nil {
		return err
	}
	lock, err := namespace.inspectRegular(namespace.lockName)
	if err != nil || lock != record.Ticket.Lock {
		return errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	if err := namespace.requireEntryAbsent(filepath.Base(credentialEndpointSidecarPath(namespace.path))); err != nil {
		return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
	}
	final, finalExists, err := namespace.inspectOptionalSocket(namespace.finalName)
	if err != nil {
		return err
	}
	quarantine, quarantineExists, err := namespace.inspectOptionalSocket(record.Ticket.QuarantineName)
	if err != nil {
		return err
	}
	wantLinked := record.Ticket.Socket
	wantLinked.Links = 2
	valid := (!finalExists && quarantineExists && quarantine == record.Ticket.Socket) ||
		(finalExists && quarantineExists && final == wantLinked && quarantine == wantLinked) ||
		(finalExists && !quarantineExists && final == record.Ticket.Socket)
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
