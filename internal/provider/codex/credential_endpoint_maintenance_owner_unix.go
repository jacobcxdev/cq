//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"golang.org/x/sys/unix"
)

type credentialEndpointMaintenanceOpenGate struct {
	initialised    bool
	activated      bool
	record         credentialEndpointMaintenanceJournal
	recordIdentity fsutil.SecureFileIdentity
	recordDigest   string
	finalised      bool
}

type credentialEndpointMaintenanceRecordProof struct {
	record   credentialEndpointMaintenanceJournal
	identity fsutil.SecureFileIdentity
	digest   string
}

type LegacyCredentialEndpointFinaliseRPCArgs struct {
	Ticket          LegacyCredentialEndpointTransitionTicket
	OwnerGeneration string
}

type LegacyCredentialEndpointFinaliseRPCReply struct {
	Finalised bool
}

func FinaliseLegacyCredentialEndpointTransition(ctx context.Context, path string, ticket LegacyCredentialEndpointTransitionTicket) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ticket.validate(); err != nil || ticket.Path != path {
		return errors.Join(ErrCredentialEndpointMaintenanceTicketMismatch, err)
	}
	client, err := dialCredentialOwner(path, credentialEndpointDialTimeout)
	if err != nil {
		return finaliseLegacyCredentialEndpointTransitionOffline(ctx, path, ticket)
	}
	defer client.Close()
	var ping CredentialEndpointPingReply
	if err := client.Call("CredentialEndpoint.Ping", CredentialEndpointPingArgs{ProtocolVersion: credentialEndpointProtocolVersion}, &ping); err != nil {
		return ErrCredentialOwnerStale
	}
	if ping.ProtocolVersion != credentialEndpointProtocolVersion || !validCredentialEndpointMaintenanceHex(ping.Generation, 16) {
		return ErrCredentialEndpointIncompatible
	}
	var reply LegacyCredentialEndpointFinaliseRPCReply
	if err := client.Call("CredentialEndpoint.FinaliseMaintenance", LegacyCredentialEndpointFinaliseRPCArgs{
		Ticket: ticket, OwnerGeneration: ping.Generation,
	}, &reply); err != nil {
		mapped := credentialEndpointMaintenanceRPCError(err)
		if errors.Is(mapped, ErrCredentialEndpointMaintenanceTicketMismatch) ||
			errors.Is(mapped, ErrCredentialEndpointMaintenanceVerifierRequired) ||
			errors.Is(mapped, ErrCredentialEndpointMaintenanceVerification) {
			return mapped
		}
		deadline := time.Now().Add(credentialEndpointDialTimeout)
		for {
			reconcileErr := finaliseLegacyCredentialEndpointTransitionOffline(ctx, path, ticket)
			if reconcileErr == nil {
				return nil
			}
			if !errors.Is(reconcileErr, fsutil.ErrExclusiveLockHeld) || time.Now().After(deadline) {
				return mapped
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			time.Sleep(min(2*time.Millisecond, time.Until(deadline)))
		}
	}
	if !reply.Finalised {
		return ErrCredentialEndpointMaintenanceConflict
	}
	return ctx.Err()
}

func finaliseLegacyCredentialEndpointTransitionOffline(ctx context.Context, path string, ticket LegacyCredentialEndpointTransitionTicket) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	namespace, err := openLegacyCredentialMaintenanceNamespace(path, ticket.Directory)
	if err != nil {
		return err
	}
	defer namespace.Close()
	if journal, exists, err := namespace.readOptionalRecord(namespace.journalName); err != nil || exists {
		if exists && journal.Ticket != ticket {
			return ErrCredentialEndpointMaintenanceTicketMismatch
		}
		return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
	}
	record, exists, err := namespace.readOptionalRecord(namespace.rollbackName)
	if err != nil {
		return err
	}
	lockProof, err := namespace.inspectRegular(namespace.lockName)
	if err != nil || lockProof != ticket.Lock {
		return errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	if !exists {
		if err := namespace.requireEntryAbsent(ticket.QuarantineName); err != nil {
			return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
		}
		return nil
	}
	if record.State != CredentialEndpointMaintenanceFinalising || record.Ticket != ticket || record.Owner == nil {
		return ErrCredentialEndpointMaintenanceConflict
	}
	lock, err := namespace.openExistingLock(ticket.Lock)
	if err != nil {
		return err
	}
	defer lock.Close()
	current, currentExists, err := namespace.readOptionalRecord(namespace.rollbackName)
	if err != nil || !currentExists || !credentialEndpointMaintenanceJournalsEqual(current, record) {
		return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
	}
	if err := namespace.validateHeldLock(ticket.Lock); err != nil {
		return err
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
		path: path, finalName: namespace.finalName, lock: lock,
		lockIdentity: fsutil.SecureFileIdentity{Device: ticket.Lock.Device, Inode: ticket.Lock.Inode, Links: ticket.Lock.Links},
		maintenanceGate: credentialEndpointMaintenanceOpenGate{
			initialised: true, activated: true, record: record,
		},
	}
	if err := candidate.validateMaintenanceOwnerShape(record, false); err != nil {
		return err
	}
	_, finalExists, err := namespace.inspectOptionalSocket(namespace.finalName)
	if err != nil {
		return err
	}
	if finalExists {
		client, protocol, _, probeErr := probeCredentialOwnerAttempt(path, credentialEndpointDialTimeout, net.DialTimeout)
		if client != nil {
			_ = client.Close()
			return ErrCredentialEndpointLockHeld
		}
		if protocol != credentialOwnerRefused {
			return errors.Join(ErrCredentialEndpointMaintenanceConflict, probeErr)
		}
	}
	quarantine, quarantineExists, err := namespace.inspectOptionalSocket(ticket.QuarantineName)
	if err != nil || (quarantineExists && quarantine != ticket.Socket) {
		return errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	if quarantineExists {
		if err := namespace.validateRecord(namespace.rollbackName, record); err != nil {
			return err
		}
		if err := namespace.validateHeldLock(ticket.Lock); err != nil {
			return err
		}
		if err := candidate.validateMaintenanceOwnerShape(record, false); err != nil {
			return err
		}
		if err := unix.Unlinkat(namespace.directoryFD, ticket.QuarantineName, 0); err != nil {
			return err
		}
		if err := namespace.directory.Sync(); err != nil {
			return err
		}
	}
	return namespace.removeRecord(namespace.rollbackName, record, func() error {
		if err := namespace.requireEntryAbsent(namespace.journalName); err != nil {
			return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
		}
		if err := namespace.requireEntryAbsent(ticket.QuarantineName); err != nil {
			return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
		}
		if err := namespace.validateHeldLock(ticket.Lock); err != nil {
			return err
		}
		return candidate.validateMaintenanceOwnerShape(record, false)
	})
}

func (r *credentialEndpointRPC) FinaliseMaintenance(args LegacyCredentialEndpointFinaliseRPCArgs, reply *LegacyCredentialEndpointFinaliseRPCReply) error {
	if r == nil || r.control == nil || r.endpoint == nil || reply == nil {
		return ErrCredentialEndpointMaintenanceConflict
	}
	if err := args.Ticket.validate(); err != nil || args.Ticket.Path != r.endpoint.path ||
		!validCredentialEndpointMaintenanceHex(args.OwnerGeneration, 16) || args.OwnerGeneration != r.generation {
		return ErrCredentialEndpointMaintenanceTicketMismatch
	}
	operation, err := r.control.BeginOwnerOperation()
	if err != nil {
		return err
	}
	defer operation.Release()
	if err := r.endpoint.finaliseActivatedMaintenance(context.Background(), args.Ticket, args.OwnerGeneration, r.finaliseVerifier); err != nil {
		return credentialEndpointMaintenanceRPCServerError(err)
	}
	reply.Finalised = true
	return nil
}

func credentialEndpointMaintenanceRPCServerError(err error) error {
	for _, typed := range []error{
		ErrCredentialEndpointMaintenanceTicketMismatch,
		ErrCredentialEndpointMaintenanceVerifierRequired,
		ErrCredentialEndpointMaintenanceVerification,
		ErrCredentialEndpointMaintenanceConflict,
		ErrCredentialOwnerRevoked,
	} {
		if errors.Is(err, typed) {
			return typed
		}
	}
	return ErrCredentialEndpointMaintenanceConflict
}

func credentialEndpointMaintenanceRPCError(err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	for _, typed := range []error{
		ErrCredentialEndpointMaintenanceTicketMismatch,
		ErrCredentialEndpointMaintenanceVerifierRequired,
		ErrCredentialEndpointMaintenanceVerification,
		ErrCredentialEndpointMaintenanceConflict,
		ErrCredentialOwnerRevoked,
	} {
		if message == typed.Error() {
			return typed
		}
	}
	return ErrCredentialEndpointMaintenanceConflict
}

func (e *credentialEndpoint) validateMaintenanceOpenGate() error {
	if e == nil {
		return ErrCredentialEndpointMaintenancePending
	}
	proof, exists, err := e.inspectActivatedMaintenanceReceipt()
	if err != nil {
		return errors.Join(ErrCredentialEndpointMaintenancePending, err)
	}
	record := proof.record

	e.maintenanceMu.Lock()
	defer e.maintenanceMu.Unlock()
	gate := &e.maintenanceGate
	if !gate.initialised {
		gate.initialised = true
		if !exists {
			return nil
		}
		if record.Owner != nil {
			if err := e.validateMaintenanceOwnerShape(record, true); err != nil {
				return errors.Join(ErrCredentialEndpointMaintenancePending, err)
			}
		}
		gate.activated = true
		gate.setRecordProof(proof)
		e.invokePhase(credentialEndpointPhaseMaintenanceAdmitted)
		return nil
	}
	if !gate.activated {
		if exists {
			return ErrCredentialEndpointMaintenancePending
		}
		return nil
	}
	if !exists {
		if gate.finalised {
			return nil
		}
		return ErrCredentialEndpointMaintenancePending
	}
	if credentialEndpointMaintenanceJournalsEqual(record, gate.record) {
		if !gate.matchesRecordProof(proof) {
			return ErrCredentialEndpointMaintenancePending
		}
		if record.Owner == nil && e.lock != nil {
			if err := e.requireMaintenanceCandidateArtifactsAbsent(); err != nil {
				return errors.Join(ErrCredentialEndpointMaintenancePending, err)
			}
		} else if record.Owner != nil {
			if err := e.validateMaintenanceOwnerShape(record, true); err != nil {
				return errors.Join(ErrCredentialEndpointMaintenancePending, err)
			}
		}
		return nil
	}
	if record.State != CredentialEndpointMaintenanceActivated || record.Ticket != gate.record.Ticket ||
		record.TicketHash != gate.record.TicketHash || record.Generation != gate.record.Generation+1 ||
		record.Owner == nil {
		return ErrCredentialEndpointMaintenancePending
	}
	if err := e.validateMaintenanceOwnerShape(record, true); err != nil {
		return errors.Join(ErrCredentialEndpointMaintenancePending, err)
	}
	gate.setRecordProof(proof)
	return nil
}

func (e *credentialEndpoint) hasUnboundActivatedMaintenanceGate() bool {
	if e == nil {
		return false
	}
	e.maintenanceMu.Lock()
	defer e.maintenanceMu.Unlock()
	return e.maintenanceGate.initialised && e.maintenanceGate.activated &&
		!e.maintenanceGate.finalised && e.maintenanceGate.record.Owner == nil
}

func (e *credentialEndpoint) hasBoundActivatedMaintenanceGate() bool {
	if e == nil {
		return false
	}
	e.maintenanceMu.Lock()
	defer e.maintenanceMu.Unlock()
	return e.maintenanceGate.initialised && e.maintenanceGate.activated &&
		!e.maintenanceGate.finalised && e.maintenanceGate.record.Owner != nil
}

func (e *credentialEndpoint) inspectActivatedMaintenanceReceipt() (credentialEndpointMaintenanceRecordProof, bool, error) {
	proof, exists, err := e.readMaintenanceRollbackRecordProof()
	if err != nil || !exists {
		return proof, exists, err
	}
	record := proof.record
	if record.State != CredentialEndpointMaintenanceActivated {
		return credentialEndpointMaintenanceRecordProof{}, true, ErrCredentialEndpointMaintenanceConflict
	}
	namespace, err := e.maintenanceNamespaceView(record.Ticket)
	if err != nil {
		return credentialEndpointMaintenanceRecordProof{}, true, err
	}
	if err := namespace.validateActivatedRecord(record, false); err != nil {
		return credentialEndpointMaintenanceRecordProof{}, true, err
	}
	current, err := namespace.inspectRegular(namespace.rollbackName)
	if err != nil || current.Device != proof.identity.Device || current.Inode != proof.identity.Inode || current.Links != proof.identity.Links {
		return credentialEndpointMaintenanceRecordProof{}, true, errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	return proof, true, nil
}

func (e *credentialEndpoint) readMaintenanceRollbackRecord() (credentialEndpointMaintenanceJournal, bool, error) {
	proof, exists, err := e.readMaintenanceRollbackRecordProof()
	return proof.record, exists, err
}

func (e *credentialEndpoint) readMaintenanceRollbackRecordProof() (credentialEndpointMaintenanceRecordProof, bool, error) {
	journalName := filepath.Base(credentialEndpointMaintenanceJournalPath(e.path))
	var stat unix.Stat_t
	if err := unix.Fstatat(e.directoryFD, journalName, &stat, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		return credentialEndpointMaintenanceRecordProof{}, false, ErrCredentialEndpointMaintenancePending
	} else if !errors.Is(err, unix.ENOENT) {
		return credentialEndpointMaintenanceRecordProof{}, false, err
	}
	rollbackName := filepath.Base(credentialEndpointMaintenanceRollbackPath(e.path))
	data, identity, err := fsutil.ReadSecureFileInDirectoryWithIdentity(
		e.fs, e.secureDirectory, rollbackName, legacyCredentialEndpointProofMaxBytes,
	)
	if errors.Is(err, os.ErrNotExist) {
		return credentialEndpointMaintenanceRecordProof{}, false, nil
	}
	if err != nil {
		return credentialEndpointMaintenanceRecordProof{}, true, err
	}
	record, err := decodeCredentialEndpointMaintenanceJournal(data)
	if err != nil || record.Ticket.Path != e.path {
		return credentialEndpointMaintenanceRecordProof{}, true, errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
	}
	namespace, err := e.maintenanceNamespaceView(record.Ticket)
	if err != nil {
		return credentialEndpointMaintenanceRecordProof{}, true, err
	}
	named, err := namespace.inspectRegular(namespace.rollbackName)
	if err != nil || named.Device != identity.Device || named.Inode != identity.Inode || named.Links != identity.Links {
		return credentialEndpointMaintenanceRecordProof{}, true, errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	return credentialEndpointMaintenanceRecordProof{record: record, identity: identity, digest: digestMaintenanceBytes(data)}, true, nil
}

func (gate *credentialEndpointMaintenanceOpenGate) setRecordProof(proof credentialEndpointMaintenanceRecordProof) {
	gate.record = proof.record
	gate.recordIdentity = proof.identity
	gate.recordDigest = proof.digest
}

func (gate credentialEndpointMaintenanceOpenGate) matchesRecordProof(proof credentialEndpointMaintenanceRecordProof) bool {
	return credentialEndpointMaintenanceRecordProofEqual(credentialEndpointMaintenanceRecordProof{
		record: gate.record, identity: gate.recordIdentity, digest: gate.recordDigest,
	}, proof)
}

func credentialEndpointMaintenanceRecordProofEqual(left, right credentialEndpointMaintenanceRecordProof) bool {
	return credentialEndpointMaintenanceJournalsEqual(left.record, right.record) &&
		left.identity == right.identity && left.digest == right.digest
}

func (e *credentialEndpoint) validateMaintenanceReceiptProof(gate credentialEndpointMaintenanceOpenGate) error {
	proof, exists, err := e.readMaintenanceRollbackRecordProof()
	if err != nil || !exists || !gate.matchesRecordProof(proof) {
		return errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	return nil
}

func (e *credentialEndpoint) maintenanceNamespaceView(ticket LegacyCredentialEndpointTransitionTicket) (*legacyCredentialMaintenanceNamespace, error) {
	if e == nil || ticket.Path != e.path {
		return nil, ErrCredentialEndpointMaintenanceTicketMismatch
	}
	proof, err := inspectLegacyCredentialDirectory(e.fs, e.secureDirectory, e.directoryFD, e.directory)
	if err != nil {
		return nil, err
	}
	if legacyCredentialEndpointDirectoryDifference(proof, ticket.Directory) != "" {
		return nil, ErrCredentialEndpointMaintenanceSnapshotChanged
	}
	namespace := &legacyCredentialMaintenanceNamespace{
		fs: e.fs, directory: e.secureDirectory, directoryFD: e.directoryFD,
		directoryPath: e.directory, directoryProof: proof, path: e.path, finalName: e.finalName,
		lockName:     filepath.Base(credentialEndpointLockPath(e.path)),
		journalName:  filepath.Base(credentialEndpointMaintenanceJournalPath(e.path)),
		rollbackName: filepath.Base(credentialEndpointMaintenanceRollbackPath(e.path)),
	}
	lock, err := namespace.inspectRegular(namespace.lockName)
	if err != nil || lock != ticket.Lock {
		return nil, errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	return namespace, namespace.validateDirectory()
}

func (e *credentialEndpoint) requireMaintenanceCandidateArtifactsAbsent() error {
	for _, name := range []string{e.finalName, filepath.Base(credentialEndpointSidecarPath(e.path))} {
		var stat unix.Stat_t
		err := unix.Fstatat(e.directoryFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
		if errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			return err
		}
		return errors.Join(ErrCredentialEndpointMaintenanceConflict, os.ErrExist)
	}
	return nil
}

func (e *credentialEndpoint) maintenanceSidecarProof() (credentialEndpointSidecar, LegacyCredentialEndpointIdentity, []byte, error) {
	name := filepath.Base(credentialEndpointSidecarPath(e.path))
	data, readIdentity, err := fsutil.ReadSecureFileInDirectoryWithIdentity(
		e.fs, e.secureDirectory, name, credentialEndpointSidecarMaxBytes,
	)
	if err != nil {
		return credentialEndpointSidecar{}, LegacyCredentialEndpointIdentity{}, nil, err
	}
	namespace, err := e.maintenanceNamespaceView(e.maintenanceGate.record.Ticket)
	if err != nil {
		return credentialEndpointSidecar{}, LegacyCredentialEndpointIdentity{}, nil, err
	}
	proof, err := namespace.inspectRegular(name)
	if err != nil || proof.Device != readIdentity.Device || proof.Inode != readIdentity.Inode || proof.Links != readIdentity.Links {
		return credentialEndpointSidecar{}, LegacyCredentialEndpointIdentity{}, nil, errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	sidecar, err := decodeCredentialEndpointSidecar(data, e.path)
	if err != nil {
		return credentialEndpointSidecar{}, LegacyCredentialEndpointIdentity{}, nil, err
	}
	return sidecar, proof, data, nil
}

func (e *credentialEndpoint) validateMaintenanceOwnerShape(record credentialEndpointMaintenanceJournal, requirePublished bool) error {
	if record.Owner == nil {
		return ErrCredentialEndpointMaintenanceConflict
	}
	namespace, err := e.maintenanceNamespaceView(record.Ticket)
	if err != nil {
		return err
	}
	final, finalExists, err := namespace.inspectOptionalSocket(namespace.finalName)
	if err != nil {
		return err
	}
	sidecar, sidecarExists, err := e.readSidecar()
	if err != nil {
		return err
	}
	if !finalExists && !sidecarExists {
		if requirePublished {
			return ErrCredentialEndpointMaintenanceConflict
		}
		return nil
	}
	if !sidecarExists || sidecar.Generation != record.Owner.Generation {
		return ErrCredentialEndpointIdentityChanged
	}
	name := filepath.Base(credentialEndpointSidecarPath(e.path))
	data, readIdentity, err := fsutil.ReadSecureFileInDirectoryWithIdentity(
		e.fs, e.secureDirectory, name, credentialEndpointSidecarMaxBytes,
	)
	if err != nil {
		return err
	}
	proof, err := namespace.inspectRegular(name)
	if err != nil || proof.Device != readIdentity.Device || proof.Inode != readIdentity.Inode || proof.Links != readIdentity.Links {
		return errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	wantSocket := record.Owner.Socket
	switch sidecar.State {
	case credentialEndpointPublished:
		if !finalExists || final != wantSocket || proof != record.Owner.SidecarFile || digestMaintenanceBytes(data) != record.Owner.SidecarSHA256 {
			return ErrCredentialEndpointIdentityChanged
		}
	case credentialEndpointClosing:
		if requirePublished || (finalExists && final != wantSocket) {
			return ErrCredentialEndpointIdentityChanged
		}
		published := sidecar
		published.State = credentialEndpointPublished
		publishedData, marshalErr := json.Marshal(published)
		if marshalErr != nil || digestMaintenanceBytes(publishedData) != record.Owner.SidecarSHA256 {
			return errors.Join(ErrCredentialEndpointIdentityChanged, marshalErr)
		}
	default:
		return ErrCredentialEndpointIdentityChanged
	}
	if !maintenanceSidecarMatchesOwnerSocket(sidecar, wantSocket) {
		return ErrCredentialEndpointIdentityChanged
	}
	return namespace.validateRecord(namespace.rollbackName, record)
}

func maintenanceSidecarMatchesOwnerSocket(sidecar credentialEndpointSidecar, proof LegacyCredentialEndpointIdentity) bool {
	return sidecar.Device == proof.Device && sidecar.Inode == proof.Inode && sidecar.UID == proof.UID &&
		sidecar.Type == proof.Type && sidecar.Mode == proof.Mode && proof.Links == 1
}

func digestMaintenanceBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func (e *credentialEndpoint) bindActivatedMaintenanceOwner() error {
	e.maintenanceMu.Lock()
	defer e.maintenanceMu.Unlock()
	gate := &e.maintenanceGate
	if !gate.initialised || !gate.activated {
		return nil
	}
	if gate.finalised {
		return ErrCredentialEndpointMaintenanceConflict
	}
	namespace, err := e.maintenanceNamespaceView(gate.record.Ticket)
	if err != nil {
		return err
	}
	if err := namespace.requireEntryAbsent(namespace.journalName); err != nil {
		return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
	}
	if err := namespace.validateRecord(namespace.rollbackName, gate.record); err != nil {
		return err
	}
	if err := e.validateMaintenanceReceiptProof(*gate); err != nil {
		return err
	}
	quarantine, err := namespace.inspectSocket(gate.record.Ticket.QuarantineName, 1)
	if err != nil || quarantine != gate.record.Ticket.Socket {
		return errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	lockProof, err := namespace.inspectHeldLock(e.lock)
	if err != nil || lockProof != gate.record.Ticket.Lock {
		return errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	finalProof, err := namespace.inspectSocket(namespace.finalName, 1)
	if err != nil {
		return err
	}
	name := filepath.Base(credentialEndpointSidecarPath(e.path))
	data, readIdentity, err := fsutil.ReadSecureFileInDirectoryWithIdentity(
		e.fs, e.secureDirectory, name, credentialEndpointSidecarMaxBytes,
	)
	if err != nil {
		return err
	}
	sidecarProof, err := namespace.inspectRegular(name)
	if err != nil || sidecarProof.Device != readIdentity.Device || sidecarProof.Inode != readIdentity.Inode || sidecarProof.Links != readIdentity.Links {
		return errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	sidecar, err := decodeCredentialEndpointSidecar(data, e.path)
	if err != nil || sidecar.State != credentialEndpointPublished || !credentialEndpointSidecarsEqual(sidecar, e.sidecar) ||
		sidecar.Generation != e.generation || !maintenanceSidecarMatchesOwnerSocket(sidecar, finalProof) {
		return errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	updated := gate.record
	updated.Generation++
	updated.Owner = &credentialEndpointMaintenanceOwnerProof{
		Generation: sidecar.Generation, Socket: finalProof, SidecarFile: sidecarProof,
		SidecarSHA256: digestMaintenanceBytes(data),
	}
	if err := namespace.writeRecord(namespace.rollbackName, updated, func() error {
		if err := namespace.requireEntryAbsent(namespace.journalName); err != nil {
			return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
		}
		if err := namespace.validateRecord(namespace.rollbackName, gate.record); err != nil {
			return err
		}
		if err := e.validateMaintenanceReceiptProof(*gate); err != nil {
			return err
		}
		currentLock, err := namespace.inspectHeldLock(e.lock)
		if err != nil || currentLock != gate.record.Ticket.Lock {
			return errors.Join(ErrCredentialEndpointIdentityChanged, err)
		}
		currentFinal, err := namespace.inspectSocket(namespace.finalName, 1)
		if err != nil || currentFinal != finalProof {
			return errors.Join(ErrCredentialEndpointIdentityChanged, err)
		}
		currentData, currentIdentity, err := fsutil.ReadSecureFileInDirectoryWithIdentity(
			e.fs, e.secureDirectory, name, credentialEndpointSidecarMaxBytes,
		)
		if err != nil || currentIdentity.Device != readIdentity.Device || currentIdentity.Inode != readIdentity.Inode ||
			currentIdentity.Links != readIdentity.Links || digestMaintenanceBytes(currentData) != digestMaintenanceBytes(data) {
			return errors.Join(ErrCredentialEndpointIdentityChanged, err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("bind activated maintenance owner: %w", err)
	}
	updatedProof, exists, err := e.readMaintenanceRollbackRecordProof()
	if err != nil || !exists || !credentialEndpointMaintenanceJournalsEqual(updatedProof.record, updated) {
		return errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	gate.setRecordProof(updatedProof)
	return nil
}

func (e *credentialEndpoint) validateMaintenanceDelegation(sidecar credentialEndpointSidecar, generation string) error {
	e.maintenanceMu.Lock()
	defer e.maintenanceMu.Unlock()
	gate := e.maintenanceGate
	if !gate.initialised || !gate.activated {
		return nil
	}
	if gate.record.Owner == nil || generation != gate.record.Owner.Generation || sidecar.Generation != generation {
		return ErrCredentialEndpointMaintenancePending
	}
	proof, exists, err := e.inspectActivatedMaintenanceReceipt()
	if err != nil || !exists || !gate.matchesRecordProof(proof) {
		return errors.Join(ErrCredentialEndpointMaintenancePending, err)
	}
	return e.validateMaintenanceOwnerShape(proof.record, true)
}

func (e *credentialEndpoint) finaliseActivatedMaintenance(ctx context.Context, ticket LegacyCredentialEndpointTransitionTicket, generation string, verifier LegacyMaintenanceFinaliseVerifier) error {
	e.maintenanceMu.Lock()
	defer e.maintenanceMu.Unlock()
	gate := &e.maintenanceGate
	if !gate.initialised || !gate.activated || gate.record.Ticket != ticket || gate.record.Owner == nil ||
		gate.record.Owner.Generation != generation {
		return ErrCredentialEndpointMaintenanceTicketMismatch
	}
	if gate.finalised {
		return nil
	}
	proof, exists, err := e.readMaintenanceRollbackRecordProof()
	if err != nil {
		return err
	}
	if !exists {
		return ErrCredentialEndpointMaintenanceConflict
	}
	record := proof.record
	if record.Ticket != ticket || record.Owner == nil || record.Owner.Generation != generation {
		return ErrCredentialEndpointMaintenanceTicketMismatch
	}
	namespace, err := e.maintenanceNamespaceView(ticket)
	if err != nil {
		return err
	}
	lockProof, err := namespace.inspectHeldLock(e.lock)
	if err != nil || lockProof != ticket.Lock {
		return errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}

	switch record.State {
	case CredentialEndpointMaintenanceActivated:
		if !gate.matchesRecordProof(proof) {
			return ErrCredentialEndpointMaintenanceConflict
		}
		if err := e.validateMaintenanceOwnerShape(record, true); err != nil {
			return err
		}
		quarantine, err := namespace.inspectSocket(ticket.QuarantineName, 1)
		if err != nil || quarantine != ticket.Socket {
			return errors.Join(ErrCredentialEndpointIdentityChanged, err)
		}
		if verifier == nil {
			return ErrCredentialEndpointMaintenanceVerifierRequired
		}
		current, currentExists, err := e.readMaintenanceRollbackRecordProof()
		if err != nil || !currentExists || !credentialEndpointMaintenanceRecordProofEqual(current, proof) {
			return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
		}
		if err := e.validateMaintenanceOwnerShape(record, true); err != nil {
			return err
		}
		finalising := record
		finalising.Generation++
		finalising.State = CredentialEndpointMaintenanceFinalising
		if err := namespace.writeRecord(namespace.rollbackName, finalising, func() error {
			if err := namespace.requireEntryAbsent(namespace.journalName); err != nil {
				return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
			}
			if err := namespace.validateRecord(namespace.rollbackName, record); err != nil {
				return err
			}
			if err := e.validateMaintenanceReceiptProof(*gate); err != nil {
				return err
			}
			currentLock, err := namespace.inspectHeldLock(e.lock)
			if err != nil || currentLock != ticket.Lock {
				return errors.Join(ErrCredentialEndpointIdentityChanged, err)
			}
			currentQuarantine, err := namespace.inspectSocket(ticket.QuarantineName, 1)
			if err != nil || currentQuarantine != ticket.Socket {
				return errors.Join(ErrCredentialEndpointIdentityChanged, err)
			}
			if err := e.validateMaintenanceOwnerShape(record, true); err != nil {
				return err
			}
			if err := verifier.VerifyLegacyMaintenanceFinalise(ctx, LegacyMaintenanceFinaliseVerification{
				TicketHash: record.TicketHash, OwnerGeneration: generation,
			}); err != nil {
				return errors.Join(ErrCredentialEndpointMaintenanceVerification, err)
			}
			return nil
		}); err != nil {
			return fmt.Errorf("write finalising maintenance receipt: %w", err)
		}
		finalisingProof, finalisingExists, err := e.readMaintenanceRollbackRecordProof()
		if err != nil || !finalisingExists || !credentialEndpointMaintenanceJournalsEqual(finalisingProof.record, finalising) {
			return errors.Join(ErrCredentialEndpointIdentityChanged, err)
		}
		record = finalising
		gate.setRecordProof(finalisingProof)
	case CredentialEndpointMaintenanceFinalising:
		if gate.record.State == CredentialEndpointMaintenanceFinalising {
			if !gate.matchesRecordProof(proof) {
				return ErrCredentialEndpointMaintenanceConflict
			}
		} else {
			expected := gate.record
			expected.Generation++
			expected.State = CredentialEndpointMaintenanceFinalising
			if !credentialEndpointMaintenanceJournalsEqual(record, expected) {
				return ErrCredentialEndpointMaintenanceConflict
			}
			gate.setRecordProof(proof)
		}
	default:
		return ErrCredentialEndpointMaintenanceConflict
	}

	quarantine, quarantineExists, err := namespace.inspectOptionalSocket(ticket.QuarantineName)
	if err != nil || (quarantineExists && quarantine != ticket.Socket) {
		return errors.Join(ErrCredentialEndpointIdentityChanged, err)
	}
	if quarantineExists {
		if err := namespace.validateRecord(namespace.rollbackName, record); err != nil {
			return err
		}
		if currentLock, err := namespace.inspectHeldLock(e.lock); err != nil || currentLock != ticket.Lock {
			return errors.Join(ErrCredentialEndpointIdentityChanged, err)
		}
		if err := unix.Unlinkat(namespace.directoryFD, ticket.QuarantineName, 0); err != nil {
			return err
		}
		if err := namespace.directory.Sync(); err != nil {
			return err
		}
	}
	if err := namespace.removeRecord(namespace.rollbackName, record, func() error {
		if err := namespace.requireEntryAbsent(namespace.journalName); err != nil {
			return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
		}
		if err := namespace.requireEntryAbsent(ticket.QuarantineName); err != nil {
			return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
		}
		currentLock, err := namespace.inspectHeldLock(e.lock)
		if err != nil || currentLock != ticket.Lock {
			return errors.Join(ErrCredentialEndpointIdentityChanged, err)
		}
		return e.validateMaintenanceOwnerShape(record, true)
	}); err != nil {
		return fmt.Errorf("remove finalised maintenance receipt: %w", err)
	}
	gate.finalised = true
	return nil
}
