package keyring

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	gokeyring "github.com/zalando/go-keyring"
)

// ClaudeAccountInspection retains source authority for one local identity.
// Credential material is internal and must never be rendered by callers.
type ClaudeAccountInspection struct {
	Account ClaudeOAuth
	Sources []string
	Active  bool
}

type claudeInspectionSource struct {
	account ClaudeOAuth
	source  string
	active  bool
}

type claudeInspectionReaders struct {
	home     string
	manifest string
	readFile func(string) ([]byte, error)
	platform func(context.Context) ([]claudeInspectionSource, error)
	get      func(string, string) (string, error)
}

// InspectClaudeAccounts reads every configured source without reconciliation,
// refresh, deduplication by email, or persistence. Missing sources are optional;
// unreadable or malformed present sources make the inventory unavailable.
func InspectClaudeAccounts(ctx context.Context) ([]ClaudeAccountInspection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	home, err := resolveCredentialHome()
	if err != nil {
		return nil, err
	}
	manifest, err := defaultCQManifestPath()
	if err != nil {
		return nil, err
	}
	return inspectClaudeAccounts(ctx, claudeInspectionReaders{
		home: home, manifest: manifest, readFile: os.ReadFile,
		platform: inspectPlatformKeychainAccounts, get: gokeyring.Get,
	})
}

var errClaudeInventory = errors.New("Claude account inventory is unavailable")

func inspectClaudeAccounts(ctx context.Context, readers claudeInspectionReaders) ([]ClaudeAccountInspection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var sources []claudeInspectionSource
	data, err := readers.readFile(filepath.Join(readers.home, ".claude", ".credentials.json"))
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, errClaudeInventory
	}
	if err == nil {
		var credentials ClaudeCredentials
		if json.Unmarshal(data, &credentials) != nil || credentials.ClaudeAiOauth == nil || credentials.ClaudeAiOauth.AccessToken == "" {
			return nil, errClaudeInventory
		}
		sources = append(sources, claudeInspectionSource{account: *credentials.ClaudeAiOauth, source: "native_client", active: true})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	platform, err := readers.platform(ctx)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		return nil, errClaudeInventory
	}
	sources = append(sources, platform...)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err = readers.readFile(readers.manifest)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, errClaudeInventory
	}
	if err == nil {
		var entries []manifestEntry
		if json.Unmarshal(data, &entries) != nil || entries == nil {
			return nil, errClaudeInventory
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if strings.TrimSpace(entry.UUID) == "" {
				return nil, errClaudeInventory
			}
			raw, err := readers.get(ServicePrefix+Hash8(entry.UUID), entry.UUID)
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			// A manifest entry declares a required credential. Its absence makes this
			// inventory incomplete rather than silently deleting the known identity.
			if err != nil {
				return nil, errClaudeInventory
			}
			var account ClaudeOAuth
			if json.Unmarshal([]byte(raw), &account) != nil || account.AccessToken == "" {
				return nil, errClaudeInventory
			}
			if account.AccountUUID != "" && account.AccountUUID != entry.UUID {
				return nil, errClaudeInventory
			}
			account.AccountUUID = entry.UUID
			sources = append(sources, claudeInspectionSource{account: account, source: "cq_managed"})
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Consolidate UUID-identified records before attributing anonymous tokens.
	// A refreshed CQ record can bridge stale native credentials and an anonymous
	// platform entry; source arrival order must not turn that one UUID into rows.
	sort.SliceStable(sources, func(i, j int) bool {
		return sources[i].account.AccountUUID != "" && sources[j].account.AccountUUID == ""
	})
	rows := []ClaudeAccountInspection{}
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		match := -1
		for i := range rows {
			a, b := rows[i].Account, source.account
			if a.AccountUUID != "" && b.AccountUUID != "" && a.AccountUUID != b.AccountUUID {
				continue
			}
			same := a.AccountUUID != "" && a.AccountUUID == b.AccountUUID ||
				a.RefreshToken != "" && a.RefreshToken == b.RefreshToken ||
				a.AccessToken != "" && a.AccessToken == b.AccessToken
			if !same && a.AccountUUID != "" && b.AccountUUID == "" {
				// Freshness consolidation may have replaced the token that proves
				// this anonymous source belongs to the same identified account.
				for _, known := range sources {
					c := known.account
					if c.AccountUUID == a.AccountUUID && ((c.AccessToken != "" && c.AccessToken == b.AccessToken) || (c.RefreshToken != "" && c.RefreshToken == b.RefreshToken)) {
						same = true
						break
					}
				}
			}
			if same && (a.AccountUUID == "" || b.AccountUUID == "") {
				// An anonymous token shared by distinct UUIDs is not identity evidence.
				ids := map[string]bool{}
				for _, known := range sources {
					c := known.account
					if c.AccountUUID != "" && ((a.AccessToken != "" && a.AccessToken == c.AccessToken) || (a.RefreshToken != "" && a.RefreshToken == c.RefreshToken) || (b.AccessToken != "" && b.AccessToken == c.AccessToken) || (b.RefreshToken != "" && b.RefreshToken == c.RefreshToken)) {
						ids[c.AccountUUID] = true
					}
				}
				same = len(ids) <= 1
			}
			if !same {
				continue
			}
			if match != -1 {
				match = -1
				break
			}
			match = i
		}
		if match == -1 {
			rows = append(rows, ClaudeAccountInspection{Account: source.account, Sources: []string{source.source}, Active: source.active})
			continue
		}
		row := &rows[match]
		email := row.Account.Email
		if email == "" {
			email = source.account.Email
		}
		if pickWinner(source.account, row.Account) {
			row.Account = mergeAccountFields(source.account, row.Account)
		} else {
			row.Account = mergeAccountFields(row.Account, source.account)
		}
		// mergeAccountFields intentionally serves legacy freshness rules and does
		// not fill email; inspection must retain safe identity metadata too.
		if row.Account.Email == "" {
			row.Account.Email = email
		}
		row.Active = row.Active || source.active
		found := false
		for _, existing := range row.Sources {
			found = found || existing == source.source
		}
		if !found {
			row.Sources = append(row.Sources, source.source)
		}
	}
	for i := range rows {
		sort.Strings(rows[i].Sources)
	}
	return rows, nil
}

// inspectClaudePlatformServices checks every supported slot, including gaps.
// The injected reader maps only an actual missing item to os.ErrNotExist.
func inspectClaudePlatformServices(ctx context.Context, read func(context.Context, string) ([]byte, error)) ([]claudeInspectionSource, error) {
	var rows []claudeInspectionSource
	for _, service := range claudePlatformServices() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, err := read(ctx, service)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, errClaudeInventory
		}
		var credentials ClaudeCredentials
		if json.Unmarshal(data, &credentials) != nil || credentials.ClaudeAiOauth == nil || credentials.ClaudeAiOauth.AccessToken == "" {
			return nil, errClaudeInventory
		}
		rows = append(rows, claudeInspectionSource{account: *credentials.ClaudeAiOauth, source: "platform_keychain"})
	}
	return rows, nil
}

func claudePlatformServices() []string {
	services := []string{"Claude Code-credentials"}
	for i := 2; i <= 10; i++ {
		services = append(services, fmt.Sprintf("Claude Code-credentials-%d", i))
	}
	return services
}
