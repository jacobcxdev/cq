package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/cache"
	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/keyring"
	"github.com/jacobcxdev/cq/internal/provider"
	claudeprov "github.com/jacobcxdev/cq/internal/provider/claude"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

type AuthRefreshAccountResult struct {
	AccountLabel       string  `json:"account_label"`
	AccountKey         *string `json:"account_key"`
	CredentialsChanged bool    `json:"credentials_changed"`
	Status             string  `json:"status"`
	Reason             string  `json:"reason"`
	ErrorCode          *string `json:"error_code"`
	Message            *string `json:"message"`
}
type AuthRefreshProviderResult struct {
	Provider           string                     `json:"provider"`
	CredentialsChanged bool                       `json:"credentials_changed"`
	ChangedCount       int                        `json:"changed_count"`
	Refreshed          int                        `json:"refreshed"`
	Unchanged          int                        `json:"unchanged"`
	ReauthRequired     int                        `json:"reauth_required"`
	Failed             int                        `json:"failed"`
	Accounts           []AuthRefreshAccountResult `json:"accounts"`
}
type authRefreshWork struct {
	err       error
	result    AuthRefreshProviderResult
	errors    []cli.Diagnostic
	succeeded bool
	unknown   bool
}
type authReauthResult struct {
	AccountUUID        string
	Email              string
	CredentialsChanged bool
}
type v2AuthDependencies struct {
	Resolve        func() error
	Now            func() time.Time
	HTTP           httputil.Doer
	DiscoverClaude func(context.Context) []keyring.ClaudeOAuth
	PersistClaude  func(context.Context, *keyring.ClaudeOAuth) (bool, error)
	Codex          func(context.Context) (codexRefreshAuthority, func() error, error)
	Login          func(context.Context) (authReauthResult, error)
	Invalidate     func(provider.ID) error
}

// Public executable dispatch is connected in T27.
func lookupV2Auth(path string) (cli.Handler, bool) { return handleV2Auth, path == "auth refresh" }
func handleV2Auth(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome {
	client := httputil.NewClient(10*time.Second, version)
	return handleV2AuthWithDependencies(ctx, inv, session, v2AuthDependencies{
		Resolve: func() error { _, err := userdirs.Default(); return err }, Now: time.Now, HTTP: client,
		DiscoverClaude: keyring.DiscoverClaudeAccountsContext,
		PersistClaude: func(ctx context.Context, a *keyring.ClaudeOAuth) (bool, error) {
			r := keyring.PersistRefreshedTokenResultContext(ctx, a)
			return r.Changed, r.Err
		},
		Codex: func(ctx context.Context) (codexRefreshAuthority, func() error, error) {
			c, err := codexprov.OpenDefaultCanonicalCredentialRefreshControl(ctx, fsutil.OSFileSystem{}, client)
			if err != nil {
				return nil, nil, err
			}
			return c.CanonicalAdmin(), c.Close, nil
		},
		Login: func(ctx context.Context) (authReauthResult, error) {
			return refreshClaudeLogin(ctx, client, func(ctx context.Context, h httputil.Doer) (*auth.TokenResponse, *auth.Profile, error) {
				return auth.LoginWithBrowser(ctx, h, func(ctx context.Context, url string) error { return auth.OpenBrowserContextTo(ctx, url, session.Err) })
			}, persistV2Claude, time.Now)
		}, Invalidate: invalidateV2AuthCache,
	})
}
func handleV2AuthWithDependencies(ctx context.Context, inv cli.Invocation, session *cli.Session, deps v2AuthDependencies) (out cli.Outcome) {
	selected := inv.Arguments["providers"]
	if len(selected) == 0 {
		selected = []string{"claude", "codex"}
	}
	seen := map[string]bool{}
	for _, p := range selected {
		if (p != "claude" && p != "codex") || seen[p] {
			return authFailure("cli_invalid_usage", "Invalid arguments: select claude and/or codex once. Run cq auth refresh --help.")
		}
		seen[p] = true
	}
	if inv.Path != "auth refresh" {
		return authFailure("cli_invalid_usage", "Invalid arguments: unexpected option. Run cq auth refresh --help.")
	}
	if ctx.Err() != nil {
		return authFailure("auth_interrupted", "")
	}
	if err := deps.Resolve(); err != nil {
		var environment *userdirs.EnvironmentError
		if errors.As(err, &environment) {
			return cli.Outcome{ExitCode: environment.ExitCode, Errors: []cli.Diagnostic{{Code: environment.Code, Message: environment.Error()}}}
		}
		return authFailure("auth_store_failed", "Cannot access credential storage.")
	}
	// Discovery warnings are safe, scoped to this invocation and never print raw errors.
	ctx = keyring.WithDiagnostics(ctx, func(code, message string) {
		out.Warnings = append(out.Warnings, cli.Diagnostic{Code: code, Message: message})
	})
	now := deps.Now()
	results := make([]AuthRefreshProviderResult, 0, len(selected))
	works := make([]authRefreshWork, 0, len(selected))
	total := 0
	for _, p := range selected {
		w := authRefreshWork{result: AuthRefreshProviderResult{Provider: p, Accounts: []AuthRefreshAccountResult{}}}
		if ctx.Err() == nil {
			if p == "claude" {
				w = refreshClaudeOutcomes(ctx, deps, now, inv.JSON, session)
			} else {
				authority, closeFn, err := deps.Codex(ctx)
				if err != nil {
					w.errors = append(w.errors, authError(codexRefreshAuthorityError(err), "unknown"))
				} else {
					w = refreshManagedCodexOutcomes(ctx, authority, now)
				}
				if closeFn != nil {
					if err := closeFn(); err != nil {
						w.errors = append(w.errors, authError(codexprov.ErrCredentialAuthorityUnavailable, "unknown"))
					}
				}
			}
		}
		finishAuthProvider(&w)
		if w.result.CredentialsChanged {
			if err := deps.Invalidate(provider.ID(p)); err != nil {
				out.Warnings = append(out.Warnings, cli.Diagnostic{Code: "auth_cache_invalidation_failed", Message: "Changed credentials were saved, but the provider quota cache could not be invalidated."})
			}
		}
		total += w.result.ChangedCount
		results = append(results, w.result)
		works = append(works, w)
	}
	var human strings.Builder
	for _, r := range results {
		fmt.Fprintf(&human, "%s: %d refreshed, %d unchanged, %d need login, %d failed\n", r.Provider, r.Refreshed, r.Unchanged, r.ReauthRequired, r.Failed)
	}
	out.Human = human.String()
	out.Data, _ = json.Marshal(struct {
		Providers          []AuthRefreshProviderResult `json:"providers"`
		CredentialsChanged bool                        `json:"credentials_changed"`
		ChangedCount       int                         `json:"changed_count"`
	}{results, total > 0, total})
	success := false
	for _, w := range works {
		success = success || w.succeeded || w.result.CredentialsChanged
	}
	for _, w := range works {
		out.Errors = append(out.Errors, w.errors...)
		if w.unknown {
			out.Warnings = append(out.Warnings, cli.Diagnostic{Code: "auth_refresh_outcome_unknown", Message: "A credential operation ended without a completion receipt; changed counts include confirmed writes only. Inspect credentials before retrying."})
		}
	}
	if ctx.Err() != nil {
		out.Errors = append(out.Errors, authDiagnostic("auth_interrupted", ""))
		out.ExitCode = 130
		return out
	}
	if len(out.Errors) == 0 {
		return out
	}
	code := "auth_refresh_failed"
	if success {
		code = "auth_refresh_partial"
	}
	// Authority and persistence failures keep their assigned status unless another provider succeeded.
	specialCode := ""
	for i, w := range works {
		for _, e := range w.errors {
			if e.Code == "auth_interrupted" {
				out.ExitCode = 130
				return out
			}
			if e.Code == "auth_authority_unavailable" || e.Code == "auth_inventory_degraded" || e.Code == "auth_store_failed" {
				otherSuccess := false
				for j, other := range works {
					if i != j && (other.succeeded || other.result.CredentialsChanged) {
						otherSuccess = true
					}
				}
				if !otherSuccess && specialCode == "" {
					specialCode = e.Code
					code = e.Code
					break
				}
			}
		}
	}
	out.ExitCode = authExit(code)
	if code == "auth_refresh_partial" || code == "auth_refresh_failed" {
		out.Errors = append([]cli.Diagnostic{authDiagnostic(code, "")}, out.Errors...)
	}
	return out
}
func invalidateV2AuthCache(id provider.ID) error {
	dir, err := cache.DefaultDir()
	if err != nil {
		return err
	}
	err = os.Remove(filepath.Join(dir, string(id)+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

var errAuthCredentialStore = errors.New("credential persistence failed")

func authExit(code string) int {
	return map[string]int{"cli_invalid_usage": 2, "auth_refresh_failed": 5, "auth_refresh_partial": 8, "auth_authority_unavailable": 4, "auth_inventory_degraded": 6, "auth_store_failed": 1, "auth_interrupted": 130}[code]
}
func authDiagnostic(code, message string) cli.Diagnostic {
	if message == "" {
		message = map[string]string{"auth_refresh_failed": "Credentials require authentication; inspect provider results.", "auth_refresh_partial": "Credential refresh completed partially; inspect provider results.", "auth_authority_unavailable": "Codex credential authority is unavailable.", "auth_inventory_degraded": "Codex credential inventory is degraded; refresh was not attempted.", "auth_interrupted": "Credential refresh interrupted."}[code]
	}
	return cli.Diagnostic{Code: code, Message: message}
}
func authFailure(code, message string) cli.Outcome {
	return cli.Outcome{ExitCode: authExit(code), Errors: []cli.Diagnostic{authDiagnostic(code, message)}}
}
func authError(err error, label string) cli.Diagnostic {
	var persistence *codexprov.RefreshPersistenceError
	switch {
	case errors.Is(err, context.Canceled):
		return authDiagnostic("auth_interrupted", "")
	case errors.Is(err, codexprov.ErrCredentialAuthorityUnavailable), errors.Is(err, codexprov.ErrCredentialControlDisabled), errors.Is(err, codexprov.ErrCredentialOwnerRevoked):
		return authDiagnostic("auth_authority_unavailable", "")
	case errors.Is(err, codexprov.ErrCredentialInventoryDegraded):
		return authDiagnostic("auth_inventory_degraded", "")
	case errors.Is(err, errAuthCredentialStore), errors.As(err, &persistence):
		return authDiagnostic("auth_store_failed", fmt.Sprintf("Cannot persist refreshed credentials for %s.", label))
	default:
		return authDiagnostic("auth_refresh_failed", "Credential refresh failed; run cq claude account login for Claude reauthentication.")
	}
}
func failedAuthAccount(w *authRefreshWork, row *AuthRefreshAccountResult, err error) {
	w.err = errors.Join(w.err, err)
	d := authError(err, row.AccountLabel)
	row.Status = "failed"
	row.Reason = "refresh_failed"
	if d.Code == "auth_store_failed" {
		row.Reason = "store_failed"
	}
	row.ErrorCode = &d.Code
	row.Message = &d.Message
	w.errors = append(w.errors, d)
	var unknown *codexprov.MutationOutcomeUnknown
	w.unknown = w.unknown || errors.As(err, &unknown)
}
func finishAuthProvider(w *authRefreshWork) {
	sort.SliceStable(w.result.Accounts, func(i, j int) bool {
		a, b := w.result.Accounts[i], w.result.Accounts[j]
		if a.AccountLabel != b.AccountLabel {
			return a.AccountLabel < b.AccountLabel
		}
		if a.AccountKey == nil {
			return b.AccountKey != nil
		}
		return b.AccountKey != nil && *a.AccountKey < *b.AccountKey
	})
	for _, a := range w.result.Accounts {
		if a.CredentialsChanged {
			w.result.ChangedCount++
		}
		switch a.Status {
		case "refreshed":
			w.result.Refreshed++
		case "unchanged":
			w.result.Unchanged++
		case "reauth_required":
			w.result.ReauthRequired++
		case "failed":
			w.result.Failed++
		}
	}
	w.result.CredentialsChanged = w.result.ChangedCount > 0
}
func refreshManagedCodexOutcomes(ctx context.Context, authority codexRefreshAuthority, now time.Time) authRefreshWork {
	w := authRefreshWork{result: AuthRefreshProviderResult{Provider: "codex", Accounts: []AuthRefreshAccountResult{}}}
	if authority == nil {
		w.err = codexprov.ErrCredentialAuthorityUnavailable
		w.errors = append(w.errors, authError(codexprov.ErrCredentialAuthorityUnavailable, "unknown"))
		return w
	}
	inventory, err := authority.List(ctx)
	if err != nil {
		w.err = codexRefreshAuthorityError(err)
		w.errors = append(w.errors, authError(codexRefreshAuthorityError(err), "unknown"))
		return w
	}
	for _, source := range inventory.ExternalSources {
		if source.ErrorCode != "" && !source.OptionalAbsent {
			w.err = codexprov.ErrCredentialInventoryDegraded
			w.errors = append(w.errors, authError(codexprov.ErrCredentialInventoryDegraded, "unknown"))
			return w
		}
	}
	for _, logical := range inventory.Accounts {
		label := logical.Identity.Email
		if label == "" {
			label = "unknown"
		}
		key := string(logical.Key)
		row := AuthRefreshAccountResult{AccountLabel: label, AccountKey: &key, Status: "unchanged", Reason: "read_only_source"}
		for _, candidate := range codexprov.ResolveCandidate(logical, "", now) {
			if ctx.Err() != nil {
				break
			}
			if candidate.Source != codexprov.SourceManaged || !candidate.CQAuthored || !candidate.RefreshEligible {
				continue
			}
			if candidate.AccessExpiresAt.IsZero() {
				if row.Status == "unchanged" {
					row.Reason = "unknown_expiry"
				}
				continue
			}
			if candidate.AccessExpiresAt.After(now.Add(time.Duration(refreshMarginMs) * time.Millisecond)) {
				if row.Status == "unchanged" {
					row.Reason = "not_expiring"
				}
				continue
			}
			result, err := authority.Refresh(ctx, candidate.Ref, candidate.Revision)
			row.CredentialsChanged = row.CredentialsChanged || result.CredentialsChanged
			if err != nil {
				failedAuthAccount(&w, &row, err)
				continue
			}
			w.succeeded = true
			if row.Status != "failed" {
				row.Status = "refreshed"
				row.Reason = "refreshed"
			}
		}
		w.result.Accounts = append(w.result.Accounts, row)
	}
	return w
}

func persistV2Claude(ctx context.Context, a *keyring.ClaudeOAuth) (bool, error) {
	r := keyring.PersistRefreshedTokenResultContext(ctx, a)
	return r.Changed, r.Err
}
func refreshClaudeLogin(ctx context.Context, client httputil.Doer, login app.ClaudeLoginFlow, persist func(context.Context, *keyring.ClaudeOAuth) (bool, error), now func() time.Time) (authReauthResult, error) {
	tokens, profile, err := login(ctx, client)
	if err != nil {
		return authReauthResult{}, errors.Join(app.ErrAccountAuthentication, err)
	}
	if tokens == nil || profile == nil || tokens.AccessToken == "" || profile.AccountUUID == "" {
		return authReauthResult{}, app.ErrAccountAuthentication
	}
	expires := tokens.ExpiresIn
	if expires <= 0 {
		expires = auth.DefaultExpiresInSec
	}
	acct := keyring.ClaudeOAuth{AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken, ExpiresAt: now().UnixMilli() + expires*1000, Scopes: strings.Fields(tokens.Scope), Email: profile.Email, AccountUUID: profile.AccountUUID, SubscriptionType: profile.Plan, RateLimitTier: profile.RateLimitTier, Profile: profile.RawJSON, TokenAccount: &keyring.TokenAccount{UUID: profile.AccountUUID, EmailAddress: profile.Email, OrganizationUUID: profile.OrgUUID}}
	changed, err := persist(ctx, &acct)
	result := authReauthResult{AccountUUID: acct.AccountUUID, Email: acct.Email, CredentialsChanged: changed}
	if err != nil {
		return result, errors.Join(errAuthCredentialStore, err)
	}
	return result, nil
}
func refreshClaudeOutcomes(ctx context.Context, deps v2AuthDependencies, now time.Time, jsonMode bool, session *cli.Session) authRefreshWork {
	w := authRefreshWork{result: AuthRefreshProviderResult{Provider: "claude", Accounts: []AuthRefreshAccountResult{}}}
	accounts := deps.DiscoverClaude(ctx)
	changes := map[string]bool{}
	failures := map[string]error{}
	accounts = syncAnonymousCredentials(ctx, deps.HTTP, accounts, now.UnixMilli(), func(a *keyring.ClaudeOAuth) {
		changed, err := deps.PersistClaude(ctx, a)
		id := claudeRefreshIdentity(*a)
		changes[id] = changes[id] || changed
		failures[id] = errors.Join(failures[id], err)
	})
	indices := map[string]int{}
	authenticated := map[string]bool{}
	put := func(id string, row AuthRefreshAccountResult) {
		if i, ok := indices[id]; ok {
			row.CredentialsChanged = row.CredentialsChanged || w.result.Accounts[i].CredentialsChanged
			w.result.Accounts[i] = row
		} else {
			indices[id] = len(w.result.Accounts)
			w.result.Accounts = append(w.result.Accounts, row)
		}
	}
	seen := map[string]bool{}
	for _, acct := range accounts {
		id := claudeRefreshIdentity(acct)
		if seen[id] || authenticated[id] {
			continue
		}
		seen[id] = true
		row := AuthRefreshAccountResult{AccountLabel: acctLabel(acct), CredentialsChanged: changes[id], Status: "unchanged", Reason: "not_expiring"}
		if err := failures[id]; err != nil {
			failedAuthAccount(&w, &row, errors.Join(errAuthCredentialStore, err))
		}
		needsLogin := false
		if ctx.Err() == nil && row.Status != "failed" {
			switch {
			case acct.ExpiresAt == 0:
				row.Reason = "unknown_expiry"
			case time.UnixMilli(acct.ExpiresAt).After(now.Add(time.Duration(refreshMarginMs) * time.Millisecond)):
				row.Reason = "not_expiring"
			case acct.RefreshToken == "":
				row.Reason = "no_refresh_token"
				needsLogin = acct.ExpiresAt <= now.UnixMilli() && (acct.Email != "" || acct.AccountUUID != "")
			default:
				rr, err := claudeprov.RefreshToken(ctx, deps.HTTP, acct.RefreshToken, acct.Scopes)
				if err != nil {
					needsLogin = true
					row.Reason = "refresh_failed"
				} else {
					acct.AccessToken = rr.AccessToken
					acct.ExpiresAt = now.UnixMilli() + rr.ExpiresIn*1000
					if rr.RefreshToken != "" {
						acct.RefreshToken = rr.RefreshToken
					}
					changed, err := deps.PersistClaude(ctx, &acct)
					row.CredentialsChanged = row.CredentialsChanged || changed
					if err != nil {
						failedAuthAccount(&w, &row, errors.Join(errAuthCredentialStore, err))
					} else {
						row.Status = "refreshed"
						row.Reason = "refreshed"
						w.succeeded = true
					}
				}
			}
		}
		if needsLogin && ctx.Err() == nil {
			row.Status = "reauth_required"
			if !jsonMode && session.Interactive {
				accepted, err := authReauthenticationPrompt(ctx, session, row.AccountLabel)
				if err != nil {
					failedAuthAccount(&w, &row, err)
				} else if !accepted {
					row.Reason = "reauthentication_skipped"
				} else {
					result, err := deps.Login(ctx)
					actualID := id
					if result.AccountUUID != "" {
						actualID = "uuid:" + result.AccountUUID
						for _, known := range accounts {
							if known.AccountUUID == result.AccountUUID || (known.AccountUUID == "" && known.Email != "" && strings.EqualFold(known.Email, result.Email)) {
								actualID = claudeRefreshIdentity(known)
								break
							}
						}
					}
					actual := row
					if actualID != id {
						label := result.Email
						if label == "" {
							label = "unknown"
						}
						actual = AuthRefreshAccountResult{AccountLabel: label}
					}
					actual.CredentialsChanged = actual.CredentialsChanged || result.CredentialsChanged
					if err != nil {
						failedAuthAccount(&w, &actual, err)
					} else {
						actual.Status = "refreshed"
						actual.Reason = "reauthenticated"
						actual.ErrorCode = nil
						actual.Message = nil
						authenticated[actualID] = true
						w.succeeded = true
					}
					if actualID == id {
						row = actual
					} else {
						put(actualID, actual)
					}
				}
			}
			if row.Status == "reauth_required" {
				d := authDiagnostic("auth_refresh_failed", "Credentials require login; run cq claude account login.")
				row.ErrorCode = &d.Code
				row.Message = &d.Message
			}
		}
		put(id, row)
	}
	// A later browser result may resolve an earlier account's authentication error.
	w.errors = nil
	for _, row := range w.result.Accounts {
		if row.ErrorCode != nil {
			w.errors = append(w.errors, cli.Diagnostic{Code: *row.ErrorCode, Message: *row.Message})
		}
	}
	return w
}

func claudeRefreshIdentity(a keyring.ClaudeOAuth) string {
	if a.AccountUUID != "" {
		return "uuid:" + a.AccountUUID
	}
	if a.Email != "" {
		return "email:" + strings.ToLower(a.Email)
	}
	return "anonymous:" + a.AccessToken
}
func authReauthenticationPrompt(ctx context.Context, s *cli.Session, label string) (bool, error) {
	if _, err := fmt.Fprintf(s.Err, "\n  Sign in as: %s\n  Press Enter to open browser (or 's' to skip): ", label); err != nil {
		return false, err
	}
	type response struct {
		accepted bool
		err      error
	}
	done := make(chan response, 1)
	go func() {
		defer func() {
			if recover() != nil {
				done <- response{err: errors.New("credential prompt failed")}
			}
		}()
		var text strings.Builder
		var one [1]byte
		for {
			_, err := io.ReadFull(s.In, one[:])
			if err != nil {
				if err == io.EOF {
					err = nil
				}
				done <- response{err: err}
				return
			}
			if one[0] == '\n' {
				done <- response{accepted: strings.TrimSpace(text.String()) != "s"}
				return
			}
			text.WriteByte(one[0])
		}
	}()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case r := <-done:
		return r.accepted, r.err
	}
}
