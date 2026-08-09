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
	ErrRefreshUnavailable = errors.New("Codex managed refresh unavailable")
	ErrRefreshIneligible  = errors.New("Codex credential lineage is not refresh eligible")
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
	operationID string
	material    CredentialMaterial
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
	if client == nil {
		return nil, errors.New("Codex refresh HTTP client unavailable")
	}
	return OpenDefaultCredentialControl(ctx, fs, func(ctx context.Context, refreshToken string) (*auth.CodexTokenResponse, error) {
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
	if result, ok, err := c.retryRetainedLocked(record); ok || err != nil {
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
	record.Metadata.OperationState = OperationRefreshing
	record.Metadata.OperationID = operationID
	if err := c.Store.Commit(&record, expected); err != nil {
		return RefreshResult{}, err
	}
	refreshingRevision := record.Metadata.Revision
	tokens, exchangeErr := c.RefreshExchange(ctx, record.Credential.RefreshToken)
	if exchangeErr != nil {
		if isDefinitiveRefreshError(exchangeErr) {
			record.Metadata.OperationState = OperationReady
			record.Metadata.OperationID = ""
			if err := c.Store.Commit(&record, refreshingRevision); err != nil {
				c.restoreRotationUncertain(&record, operationID)
				return RefreshResult{}, &RefreshPersistenceError{Err: err}
			}
		} else {
			record.Metadata.OperationState = OperationRotationUncertain
			if err := c.Store.Commit(&record, refreshingRevision); err != nil {
				return RefreshResult{}, &RefreshPersistenceError{Err: err}
			}
		}
		return RefreshResult{}, exchangeErr
	}
	if tokens == nil || tokens.AccessToken == "" {
		record.Metadata.OperationState = OperationRotationUncertain
		if err := c.Store.Commit(&record, refreshingRevision); err != nil {
			return RefreshResult{}, &RefreshPersistenceError{Err: err}
		}
		return RefreshResult{}, errors.New("Codex refresh returned empty access token")
	}

	record.Credential.AccessToken = tokens.AccessToken
	if tokens.RefreshToken != "" {
		record.Credential.RefreshToken = tokens.RefreshToken
	}
	if tokens.IDToken != "" {
		record.Credential.IDToken = tokens.IDToken
	}
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	if tokens.ExpiresIn > 0 {
		record.Document["cq_expires_at"] = now.Add(time.Duration(tokens.ExpiresIn) * time.Second).UnixMilli()
	}
	record.Document["last_refresh"] = now.UTC().Format(time.RFC3339Nano)
	record.Metadata.OperationState = OperationReady
	record.Metadata.OperationID = ""
	if err := c.Store.Commit(&record, refreshingRevision); err != nil {
		c.retainRefresh(record.Metadata.CandidateID, retainedRefresh{operationID: operationID, material: record.Credential})
		c.restoreRotationUncertain(&record, operationID)
		return RefreshResult{}, &RefreshPersistenceError{Err: err}
	}
	c.clearRetained(record.Metadata.CandidateID)
	return RefreshResult{Ref: recordRef(record), Revision: record.Metadata.Revision, Material: record.Credential}, nil
}

func (c *CredentialCoordinator) retryRetainedLocked(record ManagedRecord) (RefreshResult, bool, error) {
	c.refreshMu.Lock()
	retained, ok := c.refreshRetained[record.Metadata.CandidateID]
	c.refreshMu.Unlock()
	if !ok {
		return RefreshResult{}, false, nil
	}
	if record.Metadata.OperationState != OperationRefreshing && record.Metadata.OperationState != OperationRotationUncertain && record.Metadata.OperationState != OperationReady {
		return RefreshResult{}, true, ErrRefreshIneligible
	}
	record.Credential = retained.material
	record.Metadata.OperationState = OperationReady
	record.Metadata.OperationID = ""
	if err := c.Store.Commit(&record, record.Metadata.Revision); err != nil {
		c.restoreRotationUncertain(&record, retained.operationID)
		return RefreshResult{}, true, &RefreshPersistenceError{Err: err}
	}
	c.clearRetained(record.Metadata.CandidateID)
	return RefreshResult{Ref: recordRef(record), Revision: record.Metadata.Revision, Material: record.Credential}, true, nil
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
