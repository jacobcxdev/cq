package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

// RegistryAccount is non-secret account metadata projected for codex-auth
// interoperability.
type RegistryAccount struct {
	AccountKey string
	AccountID  string
	UserID     string
	Email      string
	Plan       string
	AuthMode   string
	CreatedAt  int64
}

// Registry owns registry.json projection. Account catalogue updates and active
// projection are deliberately separate operations.
type Registry struct {
	FS   fsutil.FileSystem
	Home string
}

func (r Registry) path() string {
	return filepath.Join(r.Home, ".codex", "accounts", "registry.json")
}

func (r Registry) read() (map[string]any, error) {
	data, err := r.FS.ReadFile(r.path())
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{"schema_version": float64(3)}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read registry: %w", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil || doc == nil {
		return nil, fmt.Errorf("parse registry: invalid JSON")
	}
	return doc, nil
}

func (r Registry) write(doc map[string]any) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal registry: %w", err)
	}
	if err := r.FS.MkdirAll(filepath.Dir(r.path()), 0o700); err != nil {
		return fmt.Errorf("create registry directory: %w", err)
	}
	if err := atomicWrite(r.FS, r.path(), data); err != nil {
		return fmt.Errorf("write registry: %w", err)
	}
	return nil
}

// UpsertAccount updates one catalogue row without changing active_account_key.
func (r Registry) UpsertAccount(account RegistryAccount) error {
	if account.AccountKey == "" {
		return errors.New("registry account key is empty")
	}
	doc, err := r.read()
	if err != nil {
		return err
	}
	accounts, _ := doc["accounts"].([]any)
	found := false
	for i, raw := range accounts {
		record, ok := raw.(map[string]any)
		if !ok || record["account_key"] != account.AccountKey {
			continue
		}
		mergeRegistryAccount(record, account)
		accounts[i] = record
		found = true
		break
	}
	if !found {
		record := make(map[string]any)
		mergeRegistryAccount(record, account)
		accounts = append(accounts, record)
	}
	doc["accounts"] = accounts
	return r.write(doc)
}

func mergeRegistryAccount(record map[string]any, account RegistryAccount) {
	record["account_key"] = account.AccountKey
	record["chatgpt_account_id"] = account.AccountID
	record["chatgpt_user_id"] = account.UserID
	record["email"] = account.Email
	record["plan"] = account.Plan
	if account.AuthMode == "" {
		account.AuthMode = "chatgpt"
	}
	record["auth_mode"] = account.AuthMode
	if _, ok := record["alias"]; !ok {
		record["alias"] = ""
	}
	if _, ok := record["created_at"]; !ok {
		if account.CreatedAt == 0 {
			account.CreatedAt = time.Now().Unix()
		}
		record["created_at"] = account.CreatedAt
	}
}

// ProjectActive changes only active-account projection. Empty key clears it.
func (r Registry) ProjectActive(accountKey string) error {
	doc, err := r.read()
	if err != nil {
		return err
	}
	if accountKey == "" {
		delete(doc, "active_account_key")
	} else {
		doc["active_account_key"] = accountKey
	}
	return r.write(doc)
}

// RemoveAccounts removes exact catalogue keys and clears their active
// projection. It preserves unrelated and unknown fields.
func (r Registry) RemoveAccounts(keys map[string]bool) error {
	if len(keys) == 0 {
		return nil
	}
	doc, err := r.read()
	if err != nil {
		return err
	}
	accounts, _ := doc["accounts"].([]any)
	kept := accounts[:0]
	for _, raw := range accounts {
		record, ok := raw.(map[string]any)
		key, _ := record["account_key"].(string)
		if ok && keys[key] {
			continue
		}
		kept = append(kept, raw)
	}
	doc["accounts"] = kept
	if active, _ := doc["active_account_key"].(string); keys[active] {
		delete(doc, "active_account_key")
	}
	return r.write(doc)
}
