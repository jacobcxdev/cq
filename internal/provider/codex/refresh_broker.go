package codex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
)

type RefreshExchange func(context.Context, string) (*auth.CodexTokenResponse, error)

type RefreshResult struct {
	Ref      CandidateRef
	Revision Revision
	Material CredentialMaterial
}

// CredentialReferenceRefresher refreshes credentials without exposing material
// outside the credential authority boundary.
type CredentialReferenceRefresher interface {
	RefreshReference(context.Context, CandidateRef, Revision) (CandidateRef, Revision, error)
}

var (
	ErrRefreshUnavailable          = errors.New("Codex managed refresh unavailable")
	ErrRefreshIneligible           = errors.New("Codex credential lineage is not refresh eligible")
	ErrRefreshOutcomeIndeterminate = errors.New("Codex refresh outcome indeterminate")
)

type RefreshPersistenceError struct{ Err error }

func (e *RefreshPersistenceError) Error() string {
	return fmt.Sprintf("persist refreshed Codex credential: %v", e.Err)
}
func (e *RefreshPersistenceError) Unwrap() error { return e.Err }

type refreshFlight struct {
	done    chan struct{}
	result  RefreshResult
	err     error
	waiters int
}

type retainedRefresh struct {
	operationID          string
	selection            RefreshMutationSelection
	commitDigest         string
	material             CredentialMaterial
	materialReady        bool
	materialCommitted    bool
	attemptPersisted     bool
	attemptIndeterminate bool
	resultPersisted      bool
	expiresIn            int64
	resultErr            error
}

func RefreshEligible(record ManagedRecord) bool {
	return record.Metadata.Version == 1 &&
		record.Metadata.Provenance == ProvenanceCQOAuth &&
		record.Metadata.RefreshOwnership == RefreshCQOwnedNeverExported &&
		record.Metadata.OperationState == OperationReady &&
		record.Metadata.OperationID == "" &&
		!record.RefreshSuspended &&
		record.Credential.RefreshToken != ""
}

func OpenDefaultCredentialRefreshControl(ctx context.Context, fs fsutil.DurableFileSystem, client httputil.Doer) (*CredentialControl, error) {
	return openDefaultCredentialRefreshControl(ctx, fs, client, OpenDefaultCredentialControl)
}

// OpenDefaultRecoveringCredentialRefreshControl is reserved for supervised
// owner startup that may replace an exactly proved stale endpoint.
func OpenDefaultRecoveringCredentialRefreshControl(ctx context.Context, fs fsutil.DurableFileSystem, client httputil.Doer) (*CredentialControl, error) {
	return openDefaultCredentialRefreshControl(ctx, fs, client, OpenDefaultRecoveringCredentialControl)
}

// OpenDefaultRecoveringCredentialRefreshControlWithLegacyMaintenanceVerifier
// is the supervised proxy-owner opener with explicit online-finalise runtime
// verification. Automatic refresh and ordinary credential paths never invoke
// the verifier.
func OpenDefaultRecoveringCredentialRefreshControlWithLegacyMaintenanceVerifier(ctx context.Context, fs fsutil.DurableFileSystem, client httputil.Doer, verifier LegacyMaintenanceFinaliseVerifier) (*CredentialControl, error) {
	return openDefaultCredentialRefreshControl(ctx, fs, client, func(ctx context.Context, fs fsutil.DurableFileSystem, exchanges ...RefreshExchange) (*CredentialControl, error) {
		return OpenDefaultRecoveringCredentialControlWithLegacyMaintenanceVerifier(ctx, fs, verifier, exchanges...)
	})
}

// OpenDefaultRecoveringCredentialRefreshControlWithLegacyMaintenanceVerifierAndRecoveryRecorder
// forwards the privacy-safe exact-recovery gate through the refresh-control
// facade used by supervised proxy startup.
func OpenDefaultRecoveringCredentialRefreshControlWithLegacyMaintenanceVerifierAndRecoveryRecorder(ctx context.Context, fs fsutil.DurableFileSystem, client httputil.Doer, verifier LegacyMaintenanceFinaliseVerifier, recorder CredentialEndpointRecoveryRecorder) (*CredentialControl, error) {
	return openDefaultCredentialRefreshControl(ctx, fs, client, func(ctx context.Context, fs fsutil.DurableFileSystem, exchanges ...RefreshExchange) (*CredentialControl, error) {
		return OpenDefaultRecoveringCredentialControlWithLegacyMaintenanceVerifierAndRecoveryRecorder(ctx, fs, verifier, recorder, exchanges...)
	})
}

type defaultCredentialControlOpener func(context.Context, fsutil.DurableFileSystem, ...RefreshExchange) (*CredentialControl, error)

func openDefaultCredentialRefreshControl(ctx context.Context, fs fsutil.DurableFileSystem, client httputil.Doer, open defaultCredentialControlOpener) (*CredentialControl, error) {
	if client == nil {
		return nil, errors.New("Codex refresh HTTP client unavailable")
	}
	return open(ctx, fs, func(ctx context.Context, refreshToken string) (*auth.CodexTokenResponse, error) {
		return auth.RefreshCodexToken(ctx, client, refreshToken)
	})
}

func (c *CredentialCoordinator) Refresh(ctx context.Context, ref CandidateRef, expected Revision) (RefreshResult, error) {
	key := string(ref.CandidateID) + ":" + string(expected)
	c.refreshMu.Lock()
	if c.refreshFlights == nil {
		c.refreshFlights = make(map[string]*refreshFlight)
	}
	if existing := c.refreshFlights[key]; existing != nil {
		existing.waiters++
		c.refreshMu.Unlock()
		select {
		case <-ctx.Done():
			return RefreshResult{}, ctx.Err()
		case <-existing.done:
			return existing.result, existing.err
		}
	}
	flight := &refreshFlight{done: make(chan struct{})}
	c.refreshFlights[key] = flight
	c.refreshMu.Unlock()
	defer func() {
		close(flight.done)
		c.refreshMu.Lock()
		delete(c.refreshFlights, key)
		c.refreshMu.Unlock()
	}()

	flight.result, flight.err = c.refreshOnce(ctx, ref, expected)
	return flight.result, flight.err
}

func (c *CredentialCoordinator) refreshOnce(ctx context.Context, ref CandidateRef, expected Revision) (RefreshResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.finishPendingRemovalLocked(ctx); err != nil {
		return RefreshResult{}, err
	}
	record, err := c.loadRef(ref)
	if err != nil {
		return RefreshResult{}, err
	}
	if record.Metadata.Revision != expected {
		return RefreshResult{}, ErrStaleRevision
	}
	if result, ok, err := c.retryRetainedLocked(ctx, record); ok || err != nil {
		return result, err
	}
	if changed, err := c.suspendSharedRefreshLocked(&record); err != nil {
		return RefreshResult{}, err
	} else if changed {
		return RefreshResult{}, ErrRefreshIneligible
	}
	if !RefreshEligible(record) {
		return RefreshResult{}, ErrRefreshIneligible
	}
	if c.RefreshExchange == nil {
		return RefreshResult{}, ErrRefreshUnavailable
	}

	operationID, err := c.Store.randomID("refresh")
	if err != nil {
		return RefreshResult{}, err
	}
	if c.RefreshMutations == nil || c.CredentialOwner == nil {
		return RefreshResult{}, errors.New("Codex refresh mutation authority unavailable")
	}
	selection, err := c.RefreshMutations.SelectRefreshMutation(operationID, ref, expected, fullRefreshMutationCapacity())
	if err != nil {
		return RefreshResult{}, err
	}
	c.retainRefresh(record.Metadata.CandidateID, retainedRefresh{operationID: operationID, selection: selection})
	result, _, err := c.retryRetainedLocked(ctx, record)
	return result, err
}

func (c *CredentialCoordinator) completeRefreshMutation(operationID, commitDigest string) error {
	receiptDigest, err := c.CredentialOwner.PublishReceipt(operationID, commitDigest)
	if err != nil {
		return err
	}
	_, err = c.RefreshMutations.CompleteRefreshMutation(operationID, receiptDigest)
	return err
}

func (c *CredentialCoordinator) retryRetainedLocked(ctx context.Context, record ManagedRecord) (RefreshResult, bool, error) {
	c.refreshMu.Lock()
	retained, ok := c.refreshRetained[record.Metadata.CandidateID]
	c.refreshMu.Unlock()
	if !ok {
		return RefreshResult{}, false, nil
	}
	if record.Metadata.OperationState != OperationRefreshing && record.Metadata.OperationState != OperationRotationUncertain && record.Metadata.OperationState != OperationReady {
		return RefreshResult{}, true, ErrRefreshIneligible
	}
	if retained.commitDigest == "" {
		commitDigest, err := c.CredentialOwner.PublishCommit(retained.operationID, retained.selection.ReservationDigest, retained.selection.CapacityLeaseDigest)
		if err != nil {
			return RefreshResult{}, true, err
		}
		retained.commitDigest = commitDigest
		c.retainRefresh(record.Metadata.CandidateID, retained)
	}
	if retained.attemptIndeterminate {
		return RefreshResult{}, true, ErrRefreshOutcomeIndeterminate
	}
	if retained.resultErr == nil && !retained.materialReady {
		if record.Metadata.OperationState == OperationReady {
			record.Metadata.OperationState = OperationRefreshing
			record.Metadata.OperationID = retained.operationID
			if err := c.Store.Commit(&record, record.Metadata.Revision); err != nil {
				return RefreshResult{}, true, &RefreshPersistenceError{Err: err}
			}
		}
		if !retained.attemptPersisted {
			if err := c.persistRefreshAttempt(retained); err != nil {
				return RefreshResult{}, true, &RefreshPersistenceError{Err: err}
			}
			retained.attemptPersisted = true
			c.retainRefresh(record.Metadata.CandidateID, retained)
		}
		tokens, exchangeErr := c.RefreshExchange(ctx, record.Credential.RefreshToken)
		if exchangeErr != nil || tokens == nil || tokens.AccessToken == "" {
			if exchangeErr == nil {
				exchangeErr = errors.New("Codex refresh returned empty access token")
			}
			retained.resultErr = exchangeErr
			c.retainRefresh(record.Metadata.CandidateID, retained)
			if err := c.persistRefreshResult(retained); err != nil {
				return RefreshResult{}, true, &RefreshPersistenceError{Err: err}
			}
			retained.resultPersisted = true
			record.Metadata.OperationState = OperationRotationUncertain
			if isDefinitiveRefreshError(exchangeErr) {
				record.Metadata.OperationState = OperationReady
				record.Metadata.OperationID = ""
			}
			if err := c.Store.Commit(&record, record.Metadata.Revision); err != nil {
				c.retainRefresh(record.Metadata.CandidateID, retained)
				return RefreshResult{}, true, &RefreshPersistenceError{Err: err}
			}
			c.retainRefresh(record.Metadata.CandidateID, retained)
		} else {
			retained.material = record.Credential
			retained.material.AccessToken = tokens.AccessToken
			if tokens.RefreshToken != "" {
				retained.material.RefreshToken = tokens.RefreshToken
			}
			if tokens.IDToken != "" {
				retained.material.IDToken = tokens.IDToken
			}
			retained.materialReady = true
			retained.expiresIn = tokens.ExpiresIn
			c.retainRefresh(record.Metadata.CandidateID, retained)
			if err := c.persistRefreshResult(retained); err != nil {
				return RefreshResult{}, true, &RefreshPersistenceError{Err: err}
			}
			retained.resultPersisted = true
			c.retainRefresh(record.Metadata.CandidateID, retained)
		}
	}
	if retained.materialReady && !retained.materialCommitted && record.Metadata.OperationState == OperationReady && record.Metadata.OperationID == "" && record.Credential == retained.material {
		retained.materialCommitted = true
		c.retainRefresh(record.Metadata.CandidateID, retained)
	}
	if retained.materialReady && !retained.materialCommitted {
		record.Credential = retained.material
		record.Metadata.OperationState = OperationReady
		record.Metadata.OperationID = ""
		now := time.Now()
		if c.Now != nil {
			now = c.Now()
		}
		record.Document["last_refresh"] = now.UTC().Format(time.RFC3339Nano)
		if retained.expiresIn > 0 {
			record.Document["cq_expires_at"] = now.Add(time.Duration(retained.expiresIn) * time.Second).UnixMilli()
		}
		if err := c.Store.Commit(&record, record.Metadata.Revision); err != nil {
			c.restoreRotationUncertain(&record, retained.operationID)
			return RefreshResult{}, true, &RefreshPersistenceError{Err: err}
		}
		retained.materialCommitted = true
		c.retainRefresh(record.Metadata.CandidateID, retained)
	}
	if err := c.completeRefreshMutation(retained.operationID, retained.commitDigest); err != nil {
		return RefreshResult{}, true, &RefreshPersistenceError{Err: err}
	}
	c.clearRetained(record.Metadata.CandidateID)
	if retained.resultErr != nil {
		return RefreshResult{}, true, retained.resultErr
	}
	return RefreshResult{Ref: recordRef(record), Revision: record.Metadata.Revision, Material: record.Credential}, true, nil
}

func (c *CredentialCoordinator) persistRefreshAttempt(retained retainedRefresh) error {
	recorder, ok := c.CredentialOwner.(CredentialOwnerRecoveryRecorder)
	if !ok {
		return nil
	}
	return recorder.PublishRefreshAttempt(retained.operationID, retained.commitDigest)
}

func (c *CredentialCoordinator) persistRefreshResult(retained retainedRefresh) error {
	if retained.resultPersisted {
		return nil
	}
	recorder, ok := c.CredentialOwner.(CredentialOwnerRecoveryRecorder)
	if !ok {
		return nil
	}
	result := CredentialOwnerRefreshResult{Material: retained.material, ExpiresIn: retained.expiresIn}
	if retained.resultErr != nil {
		result.Error = retained.resultErr.Error()
		result.Definitive = isDefinitiveRefreshError(retained.resultErr)
	}
	return recorder.PublishRefreshResult(retained.operationID, retained.commitDigest, result)
}

func (c *CredentialCoordinator) retainRefresh(candidateID CandidateID, retained retainedRefresh) {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	if c.refreshRetained == nil {
		c.refreshRetained = make(map[CandidateID]retainedRefresh)
	}
	c.refreshRetained[candidateID] = retained
}

func (c *CredentialCoordinator) clearRetained(candidateID CandidateID) {
	c.refreshMu.Lock()
	delete(c.refreshRetained, candidateID)
	c.refreshMu.Unlock()
}

func (c *CredentialCoordinator) suspendSharedRefreshLocked(record *ManagedRecord) (bool, error) {
	data, err := c.Store.FS.ReadFile(filepath.Join(c.Store.Home, ".codex", "auth.json"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	active, ok := parseAccountData(data, "")
	if !ok || active.RefreshToken == "" || active.RefreshToken != record.Credential.RefreshToken {
		return false, nil
	}
	if record.Metadata.RefreshOwnership == RefreshExportedToSystem {
		return false, nil
	}
	expected := record.Metadata.Revision
	record.Metadata.RefreshOwnership = RefreshExportedToSystem
	if err := c.Store.Commit(record, expected); err != nil {
		return false, err
	}
	return true, nil
}

func (c *CredentialCoordinator) restoreRotationUncertain(record *ManagedRecord, operationID string) {
	current, err := c.Store.Load(record.Path)
	if err != nil {
		return
	}
	if current.Metadata.OperationState == OperationRotationUncertain && current.Metadata.OperationID != "" {
		return
	}
	expected := current.Metadata.Revision
	current.Metadata.OperationState = OperationRotationUncertain
	if current.Metadata.OperationID == "" {
		current.Metadata.OperationID = operationID
	}
	_ = c.Store.Commit(&current, expected)
}

type definitiveRefreshError interface{ RefreshDefinitive() bool }

func isDefinitiveRefreshError(err error) bool {
	var definitive definitiveRefreshError
	return errors.As(err, &definitive) && definitive.RefreshDefinitive()
}
