package codex

import (
	"context"
	"fmt"
	"time"

	"github.com/jacobcxdev/cq/internal/auth"
)

// AccountCatalogue owns only non-secret account catalogue rows.
type AccountCatalogue interface {
	UpsertAccount(RegistryAccount) error
	RemoveAccounts(map[string]bool) error
}

// ManagedProjectionError reports a catalogue projection failure after durable
// managed authority may already have committed.
type ManagedProjectionError struct {
	Err error
}

func (e *ManagedProjectionError) Error() string {
	return fmt.Sprintf("managed account projection: %v", e.Err)
}

func (e *ManagedProjectionError) Unwrap() error { return e.Err }

func registryAccountFromManagedRecord(record ManagedRecord) (RegistryAccount, bool) {
	claims := auth.DecodeCodexClaims(record.Credential.IDToken)
	if claims.AccountID == "" || claims.UserID == "" || claims.RecordKey() == "" || record.Credential.AccountID != claims.AccountID {
		return RegistryAccount{}, false
	}

	var accountKey string
	switch record.Metadata.Version {
	case 1:
		accountKey = string(record.Metadata.AccountKey)
	case 0:
		accountKey = claims.RecordKey()
	default:
		return RegistryAccount{}, false
	}
	if accountKey == "" {
		return RegistryAccount{}, false
	}

	createdAt := int64(0)
	if raw, ok := record.Document["last_refresh"].(string); ok {
		if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			createdAt = parsed.Unix()
		}
	}
	authMode, _ := record.Document["auth_mode"].(string)
	return RegistryAccount{
		AccountKey: accountKey,
		AccountID:  claims.AccountID,
		UserID:     claims.UserID,
		Email:      claims.Email,
		Plan:       claims.PlanType,
		AuthMode:   authMode,
		CreatedAt:  createdAt,
	}, true
}

func (c *CredentialCoordinator) projectManagedRecordLocked(record ManagedRecord) error {
	account, ok := registryAccountFromManagedRecord(record)
	if !ok {
		return nil
	}
	if err := c.Registry.UpsertAccount(account); err != nil {
		return &ManagedProjectionError{Err: err}
	}
	return nil
}

func (c *CredentialCoordinator) recoverManagedProjectionLocked() error {
	records, err := c.managedRecords()
	if err != nil {
		return &ManagedProjectionError{Err: err}
	}
	for _, record := range records {
		if err := c.projectManagedRecordLocked(record); err != nil {
			return err
		}
	}
	return nil
}

// RecoverCredentialState completes pending removal before rebuilding derived
// non-secret catalogue rows from durable managed records.
func (c *CredentialCoordinator) RecoverCredentialState(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.finishPendingRemovalLocked(ctx); err != nil {
		return err
	}
	return c.recoverManagedProjectionLocked()
}
