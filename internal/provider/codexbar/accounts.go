// Package codexbar reads explicitly configured provider credentials without
// adopting ownership or modifying CodexBar's files or active selection.
package codexbar

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

// Account is a borrowed provider credential. Token never enters public JSON.
type Account struct {
	ID                 string `json:"id"`
	Label              string `json:"label,omitempty"`
	Token              string `json:"-"`
	ExternalIdentifier string `json:"external_identifier,omitempty"`
	UsageScope         string `json:"usage_scope,omitempty"`
	OrganizationID     string `json:"organization_id,omitempty"`
	WorkspaceID        string `json:"workspace_id,omitempty"`
	Region             string `json:"region,omitempty"`
	Active             bool   `json:"active"`
}

type tokenAccount struct {
	ID                 string `json:"id"`
	Label              string `json:"label"`
	Token              string `json:"token"`
	ExternalIdentifier string `json:"externalIdentifier"`
	UsageScope         string `json:"usageScope"`
	OrganizationID     string `json:"organizationId"`
	WorkspaceID        string `json:"workspaceID"`
}
type tokenAccounts struct {
	Version     int            `json:"version"`
	ActiveIndex int            `json:"activeIndex"`
	Accounts    []tokenAccount `json:"accounts"`
}
type providerConfig struct {
	ID            string         `json:"id"`
	Region        string         `json:"region"`
	APIKey        string         `json:"apiKey"`
	CookieHeader  string         `json:"cookieHeader"`
	TokenAccounts *tokenAccounts `json:"tokenAccounts"`
}

// ReadAccounts reads one provider's declared credentials in preferred order.
// CODEXBAR_CONFIG is authoritative. Missing files are ordinary absence; malformed
// or insecure selected sources return a classified error without source contents.
func ReadAccounts(fs fsutil.FileSystem, env func(string) string, home, provider string) ([]Account, error) {
	if fs == nil || env == nil || !filepath.IsAbs(home) || provider == "" {
		return nil, errors.New("invalid CodexBar credential source")
	}
	path, err := configPath(fs, env, home)
	if err != nil {
		return nil, err
	}
	data, err := read(fs, path)
	if err != nil {
		return nil, err
	}
	if data != nil {
		var config struct {
			Version   int              `json:"version"`
			Providers []providerConfig `json:"providers"`
		}
		if json.Unmarshal(data, &config) != nil || config.Version != 1 || config.Providers == nil {
			return nil, errors.New("invalid CodexBar credential configuration")
		}
		var selected *providerConfig
		for i := range config.Providers {
			if config.Providers[i].ID == provider {
				if selected != nil {
					return nil, errors.New("duplicate CodexBar provider")
				}
				selected = &config.Providers[i]
			}
		}
		if selected != nil {
			if selected.TokenAccounts != nil {
				return decodeAccounts(*selected.TokenAccounts, selected.Region)
			}
			token := strings.TrimSpace(selected.APIKey)
			if provider == "grok" {
				token = strings.TrimSpace(selected.CookieHeader)
			}
			if token != "" {
				return []Account{{ID: "configured", Token: token, Region: selected.Region, Active: true}}, nil
			}
		}
		// The current configuration is authoritative, including an explicit
		// absence. Do not resurrect credentials from a stale legacy file.
		return nil, nil
	}
	if strings.TrimSpace(env("CODEXBAR_CONFIG")) != "" {
		return nil, nil
	}
	// Older CodexBar releases used a shared provider-keyed token file.
	legacy := filepath.Join(home, "Library", "Application Support", "CodexBar", "token-accounts.json")
	data, err = read(fs, legacy)
	if err != nil || data == nil {
		return nil, err
	}
	var config struct {
		Version   int                      `json:"version"`
		Providers map[string]tokenAccounts `json:"providers"`
	}
	if json.Unmarshal(data, &config) != nil || config.Version != 1 || config.Providers == nil {
		return nil, errors.New("invalid CodexBar token accounts")
	}
	accounts, ok := config.Providers[provider]
	if !ok {
		return nil, nil
	}
	return decodeAccounts(accounts, "")
}

func configPath(fs fsutil.FileSystem, env func(string) string, home string) (string, error) {
	if raw := strings.TrimSpace(env("CODEXBAR_CONFIG")); raw != "" {
		if strings.HasPrefix(raw, "~/") {
			raw = filepath.Join(home, raw[2:])
		}
		if !filepath.IsAbs(raw) {
			return "", errors.New("CodexBar config path must be absolute")
		}
		return filepath.Clean(raw), nil
	}
	raw := strings.TrimSpace(env("XDG_CONFIG_HOME"))
	if strings.HasPrefix(raw, "~/") {
		raw = filepath.Join(home, raw[2:])
	}
	if filepath.IsAbs(raw) {
		return filepath.Join(raw, "codexbar", "config.json"), nil
	}
	path := filepath.Join(home, ".config", "codexbar", "config.json")
	inspector, ok := fs.(fsutil.SecurePathInspector)
	if !ok {
		return "", errors.New("secure CodexBar credential reads unavailable")
	}
	if _, err := inspector.Lstat(path); err == nil {
		return path, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("inspect CodexBar configuration")
	}
	legacy := filepath.Join(home, ".codexbar", "config.json")
	if _, err := inspector.Lstat(legacy); err == nil {
		return legacy, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("inspect CodexBar configuration")
	}
	return path, nil
}

func read(fs fsutil.FileSystem, path string) ([]byte, error) {
	inspector, ok := fs.(fsutil.SecurePathInspector)
	if !ok {
		return nil, errors.New("secure CodexBar credential reads unavailable")
	}
	if _, err := inspector.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, errors.New("inspect CodexBar credentials")
	}
	directory, err := fsutil.OpenOwnerControlledDirectory(fs, filepath.Dir(path))
	if err != nil {
		return nil, errors.New("unsafe CodexBar credential directory")
	}
	defer directory.Close()
	data, _, err := fsutil.ReadOwnerControlledFileInDirectoryWithIdentity(inspector, directory, filepath.Dir(path), filepath.Base(path), 1<<20)
	if err != nil {
		return nil, errors.New("unsafe or unreadable CodexBar credentials")
	}
	return data, nil
}

func decodeAccounts(data tokenAccounts, region string) ([]Account, error) {
	if data.Version != 1 || data.Accounts == nil {
		return nil, errors.New("invalid CodexBar token accounts")
	}
	active := data.ActiveIndex
	if active < 0 {
		active = 0
	}
	if active >= len(data.Accounts) {
		active = max(0, len(data.Accounts)-1)
	}
	accounts := make([]Account, 0, len(data.Accounts))
	seen := make(map[string]bool)
	for i, a := range data.Accounts {
		if a.ID == "" || seen[a.ID] || !publicText(a.ID) || !publicText(a.Label) || !publicText(a.ExternalIdentifier) || !publicText(a.UsageScope) || !publicText(a.OrganizationID) || !publicText(a.WorkspaceID) || !publicText(region) {
			return nil, errors.New("invalid CodexBar account metadata")
		}
		seen[a.ID] = true
		token := strings.TrimSpace(a.Token)
		if token == "" {
			continue
		}
		accounts = append(accounts, Account{ID: a.ID, Label: a.Label, Token: token, ExternalIdentifier: a.ExternalIdentifier, UsageScope: a.UsageScope, OrganizationID: a.OrganizationID, WorkspaceID: a.WorkspaceID, Region: region, Active: i == active})
	}
	for i, a := range accounts {
		if a.Active && i > 0 {
			copy(accounts[1:i+1], accounts[:i])
			accounts[0] = a
			break
		}
	}
	return accounts, nil
}

func publicText(s string) bool {
	if len(s) > 512 {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
