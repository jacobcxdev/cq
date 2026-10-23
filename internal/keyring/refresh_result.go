package keyring

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	gokeyring "github.com/zalando/go-keyring"
)

// CredentialWriteResult records actual persistence independently of later errors.
type CredentialWriteResult struct {
	Changed bool
	Err     error
}

func StoreCQAccountResultContext(ctx context.Context, acct *ClaudeOAuth) CredentialWriteResult {
	return storeCQAccountResultContext(ctx, acct, gokeyring.Set, gokeyring.Delete)
}
func storeCQAccountResultContext(ctx context.Context, acct *ClaudeOAuth, set func(string, string, string) error, remove func(string, string) error) (result CredentialWriteResult) {
	fail := func(err error) CredentialWriteResult { result.Err = err; return result }
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if acct.AccountUUID == "" {
		return fail(errors.New("account UUID required for keyring storage"))
	}
	path, err := defaultCQManifestPath()
	if err != nil {
		return fail(fmt.Errorf("resolve manifest path: %w", err))
	}
	service := ServicePrefix + Hash8(acct.AccountUUID)
	data, err := json.Marshal(acct)
	if err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if err := set(service, acct.AccountUUID, string(data)); err != nil {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		if err := remove(service, acct.AccountUUID); err == nil {
			result.Changed = true
		}
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		if err := set(service, acct.AccountUUID, string(data)); err != nil {
			return fail(err)
		}
	}
	result.Changed = true
	entries := loadManifestContext(ctx, path)
	found := false
	for i, e := range entries {
		if e.UUID == acct.AccountUUID {
			entries[i].Email = acct.Email
			found = true
			break
		}
	}
	if !found {
		entries = append(entries, manifestEntry{UUID: acct.AccountUUID, Email: acct.Email})
	}
	if err := saveManifestContext(ctx, path, entries); err != nil {
		return fail(&ClaudeStoreError{Err: err})
	}
	return result
}

type refreshPersistenceOperations struct {
	write  func(*ClaudeCredentials) error
	update func(string, *ClaudeCredentials) error
	store  func(*ClaudeOAuth) CredentialWriteResult
}

func PersistRefreshedTokenResultContext(ctx context.Context, acct *ClaudeOAuth) CredentialWriteResult {
	return persistRefreshedTokenResult(ctx, acct, refreshPersistenceOperations{
		write:  func(c *ClaudeCredentials) error { return WriteCredentialsFileContext(ctx, c) },
		update: func(s string, c *ClaudeCredentials) error { return updateKeychainEntryContext(ctx, s, c) },
		store:  func(a *ClaudeOAuth) CredentialWriteResult { return StoreCQAccountResultContext(ctx, a) },
	})
}
func persistRefreshedTokenResult(ctx context.Context, acct *ClaudeOAuth, ops refreshPersistenceOperations) (result CredentialWriteResult) {
	defer func() { result.Err = errors.Join(result.Err, ctx.Err()) }()
	if ctx.Err() != nil {
		return result
	}
	cqAccount := *acct
	home, err := resolveCredentialHome()
	if err != nil {
		result.Err = err
	} else {
		data, err := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			result.Err = err
		}
		if err == nil {
			var creds ClaudeCredentials
			if err := json.Unmarshal(data, &creds); err != nil {
				result.Err = err
			} else if canUpdateStoredAccount(creds.ClaudeAiOauth, acct) {
				updated := mergeRefreshedAccount(creds.ClaudeAiOauth, acct)
				creds.ClaudeAiOauth = &updated
				cqAccount = updated
				if ctx.Err() != nil {
					return result
				}
				if err := ops.write(&creds); err != nil {
					result.Err = errors.Join(result.Err, err)
				} else {
					result.Changed = true
					if ctx.Err() != nil {
						return result
					}
					if err := ops.update("Claude Code-credentials", &creds); err != nil {
						result.Err = errors.Join(result.Err, err)
					}
				}
			}
		}
	}
	if cqAccount.AccountUUID == "" {
		cqAccount.AccountUUID = acct.AccountUUID
	}
	if cqAccount.Email == "" {
		cqAccount.Email = acct.Email
	}
	if cqAccount.AccountUUID != "" {
		if ctx.Err() != nil {
			return result
		}
		stored := ops.store(&cqAccount)
		result.Changed = result.Changed || stored.Changed
		result.Err = errors.Join(result.Err, stored.Err)
	}
	if !result.Changed && result.Err == nil {
		result.Err = errors.New("no matching credential store")
	}
	return result
}
