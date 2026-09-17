package main

import (
	"context"
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
	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/keyring"
	"github.com/jacobcxdev/cq/internal/provider"
	claudeprov "github.com/jacobcxdev/cq/internal/provider/claude"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

type v2AccountActivation struct{ Changed, Active bool }
type v2AccountRemoval struct{ Changed, ActiveRemoved, Pending bool }
type v2AccountSelection struct {
	Account                AccountSummary
	Activatable, Removable bool
	Retained               []string
	Activate               func(context.Context) (v2AccountActivation, error)
	Remove                 func(context.Context) (v2AccountRemoval, error)
}
type v2AccountMutationDependencies struct {
	Login      func(context.Context, provider.ID, bool) (app.AccountLoginResult, error)
	Select     func(context.Context, provider.ID, string) (v2AccountSelection, error)
	Invalidate func(context.Context, provider.ID) error
}

func lookupV2AccountMutation(path string) (cli.Handler, bool) {
	switch path {
	case "claude account login", "claude account activate", "claude account remove", "codex account login", "codex account activate", "codex account remove":
		return handleV2AccountMutation, true
	}
	return nil, false
}
func handleV2AccountMutation(ctx context.Context, inv cli.Invocation, s *cli.Session) cli.Outcome {
	return handleV2AccountMutationWithDependencies(ctx, inv, s, defaultV2AccountMutationDependencies(strings.HasSuffix(inv.Path, "remove"), s.Err))
}
func handleV2AccountMutationWithDependencies(parent context.Context, inv cli.Invocation, s *cli.Session, deps v2AccountMutationDependencies) cli.Outcome {
	parts := strings.Fields(inv.Path)
	id := provider.ID(parts[0])
	action := parts[2]
	if action == "remove" && inv.Options["yes"][0] != "true" && (inv.JSON || !s.Interactive) {
		return v2MutationFailure("account_confirmation_required")
	}
	timeout, err := time.ParseDuration(inv.Options["timeout"][0])
	if err != nil {
		return v2MutationFailure("account_io_failed")
	}
	budget := cli.BeginBudget(parent, timeout, 0)
	defer budget.Close()
	ctx := budget.Work()
	if err := ctx.Err(); err != nil {
		return v2MutationError(err, "account_io_failed")
	}
	title := "Codex"
	if id == provider.Claude {
		title = "Claude"
	}
	if action == "login" {
		result, err := deps.Login(ctx, id, inv.Options["activate"][0] == "true")
		row := v2AccountSummary(id, result.Account.AccountID, result.Account.Email, result.Account.Label, result.Account.RateLimitTier)
		row.AccountReference = v2AccountString(result.Reference)
		row.Stable = row.AccountReference != nil
		if id == provider.Claude {
			row.Stable = row.AccountID != nil
		}
		row.Aliases = append([]string{}, result.Aliases...)
		row.Active = result.Account.Active
		row.Sources = append([]string{}, result.Sources...)
		sort.Strings(row.Sources)

		outcome := cli.Outcome{}
		if result.Activated && deps.Invalidate != nil {
			err = errors.Join(err, deps.Invalidate(ctx, id))
		}
		if err != nil {
			code := "account_io_failed"
			if errors.Is(err, app.ErrAccountAuthentication) {
				code = "account_auth_failed"
			}
			if result.CredentialsSaved {
				code = "account_login_postcheck_partial"
				if errors.Is(err, app.ErrAccountActivation) {
					code = "account_login_partial"
				}
			}
			outcome = v2MutationError(err, code)
		}
		if result.CredentialsSaved {
			var account *AccountSummary
			if result.AccountObserved {
				account = &row
			}
			var activated *bool
			activeText := "unknown"
			if result.ActivationKnown {
				activated = &result.Activated
				activeText = fmt.Sprintf("%t", result.Activated)
			}
			outcome.Data, _ = cli.EncodeData(struct {
				Account   *AccountSummary `json:"account"`
				Activated *bool           `json:"activated"`
				Saved     bool            `json:"credentials_saved"`
			}{account, activated, true})
			name := "Unknown account"
			if account != nil {
				name = cli.HumanValue(account.DisplayName)
			}
			outcome.Human = fmt.Sprintf("Logged in to %s as %s.\nNative client default: %s.\n", title, name, activeText)
			if errors.Is(err, app.ErrAccountActivation) && errors.Is(err, app.ErrAccountLoginPostcheck) {
				outcome.Errors = append(outcome.Errors, v2MutationFailure("account_login_postcheck_partial").Errors...)
			}
		}

		return outcome
	}
	selected, err := deps.Select(ctx, id, inv.Arguments["account"][0])
	if err != nil {
		return v2MutationError(err, "account_inventory_unavailable")
	}
	if err := ctx.Err(); err != nil {
		return v2MutationError(err, "account_timeout")
	}
	if action == "activate" {
		if !selected.Activatable {
			return v2MutationFailure("account_not_activatable")
		}
		result, err := selected.Activate(ctx)
		if result.Active && deps.Invalidate != nil {
			err = errors.Join(err, deps.Invalidate(ctx, id))
		}
		outcome := cli.Outcome{}
		if err != nil {
			code := "account_io_failed"
			if result.Changed {
				code = "account_activation_partial"
			}
			outcome = v2MutationError(err, code)
		}
		if err == nil || result.Changed {
			selected.Account.Active = result.Active
			outcome.Data, _ = cli.EncodeData(struct {
				Account AccountSummary `json:"account"`
				Changed bool           `json:"changed"`
			}{selected.Account, result.Changed})
			outcome.Human = fmt.Sprintf("%s native client default: %s.\nChanged: %t.\n", title, cli.HumanValue(selected.Account.DisplayName), result.Changed)
		}
		return outcome
	}
	if !selected.Removable {
		return v2MutationFailure("account_read_only")
	}
	result := v2AccountRemoval{}
	confirmed := inv.Options["yes"][0] == "true"
	if !confirmed {
		prompt := fmt.Sprintf("Account: %s.\nAccount reference: %s.\nNative active credentials will be removed: %t.\nRemove local credentials for %s? [y/N] ", cli.HumanValue(selected.Account.DisplayName), cli.HumanValue(*selected.Account.AccountReference), selected.Account.Active, cli.HumanValue(selected.Account.DisplayName))
		if _, err := fmt.Fprint(s.Err, prompt); err != nil {
			return v2MutationFailure("account_io_failed")
		}
		budget.Pause()
		type consentResult struct {
			confirmed bool
			err       error
		}
		done := make(chan consentResult, 1)
		go func() {
			result := consentResult{}
			defer func() {
				if recover() != nil {
					result.err = errors.New("confirmation failed")
				}
				done <- result
			}()
			result.confirmed, result.err = cli.Confirm(s, "")
		}()
		select {
		case result := <-done:
			confirmed, err = result.confirmed, result.err
		case <-parent.Done():
			err = parent.Err()
		}
		budget.Resume()
		ctx = budget.Work()
		if err != nil {
			return v2MutationError(err, "account_io_failed")
		}
		if err := ctx.Err(); err != nil {
			return v2MutationError(err, "account_timeout")
		}
	}
	if confirmed {
		result, err = selected.Remove(ctx)
		if result.Changed && deps.Invalidate != nil {
			err = errors.Join(err, deps.Invalidate(ctx, id))
		}
	}
	outcome := cli.Outcome{}
	if err != nil || result.Pending {
		code := "account_io_failed"
		if result.Changed || result.Pending {
			code = "account_removal_partial"
		}
		outcome = v2MutationError(err, code)
	}
	if err == nil || result.Changed || result.Pending {
		if selected.Retained == nil {
			selected.Retained = []string{}
		}
		outcome.Data, _ = cli.EncodeData(struct {
			Reference     string   `json:"account_reference"`
			Removed       bool     `json:"removed"`
			ActiveRemoved bool     `json:"active_credentials_removed"`
			Retained      []string `json:"retained_external_sources"`
		}{*selected.Account.AccountReference, result.Changed, result.ActiveRemoved, selected.Retained})
		retained := "none"
		if len(selected.Retained) > 0 {
			retained = cli.HumanValue(strings.Join(selected.Retained, ","))
		}
		outcome.Human = fmt.Sprintf("Local %s credentials removed: %t.\nNative active credentials removed: %t.\nRetained external sources: %s.\n", title, result.Changed, result.ActiveRemoved, retained)
	}
	return outcome
}

type v2MutationDiagnostic string

func (e v2MutationDiagnostic) Error() string { return string(e) }
func v2MutationError(err error, fallback string) cli.Outcome {
	if errors.Is(err, context.Canceled) {
		return v2MutationFailure("interrupted")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return v2MutationFailure("account_timeout")
	}
	if strings.HasSuffix(fallback, "_partial") {
		return v2MutationFailure(fallback)
	}
	var environment *userdirs.EnvironmentError
	if errors.As(err, &environment) {
		return cli.Outcome{ExitCode: environment.ExitCode, Errors: []cli.Diagnostic{{Code: environment.Code, Message: environment.Error()}}}
	}
	if errors.Is(err, codexprov.ErrCredentialAuthorityUnavailable) || errors.Is(err, codexprov.ErrCredentialControlDisabled) || errors.Is(err, codexprov.ErrCredentialOwnerRevoked) {
		return v2MutationFailure("account_inventory_unavailable")
	}
	if errors.Is(err, codexprov.ErrStaleRevision) || errors.Is(err, keyring.ErrClaudeIdentityChanged) {
		return v2MutationFailure("account_unstable")
	}
	if errors.Is(err, codexprov.ErrAccountNotActivatable) {
		return v2MutationFailure("account_not_activatable")
	}
	referenceCodes := map[string]string{"empty": "account_reference_empty", "missing": "account_not_found", "ambiguous": "account_ambiguous", "unstable": "account_unstable", "not_activatable": "account_not_activatable"}
	var codexReference *codexprov.AccountReferenceError
	if errors.As(err, &codexReference) {
		return v2MutationFailure(referenceCodes[string(codexReference.Code)])
	}
	var claudeReference *claudeprov.AccountReferenceError
	if errors.As(err, &claudeReference) {
		out := v2MutationFailure(referenceCodes[claudeReference.Code])

		return out
	}
	var diagnostic v2MutationDiagnostic
	if errors.As(err, &diagnostic) {
		return v2MutationFailure(string(diagnostic))
	}
	return v2MutationFailure(fallback)
}

func v2MutationFailure(code string) cli.Outcome {
	exits := map[string]int{"account_inventory_unavailable": 4, "account_timeout": 7, "account_io_failed": 1, "account_auth_failed": 5, "account_login_partial": 8, "account_login_postcheck_partial": 8, "account_reference_empty": 2, "account_not_found": 3, "account_ambiguous": 6, "account_unstable": 6, "account_not_activatable": 6, "account_activation_partial": 8, "account_confirmation_required": 6, "account_read_only": 6, "account_removal_partial": 8, "interrupted": 130}
	messages := map[string]string{
		"account_inventory_unavailable": "Account inventory is unavailable.", "account_timeout": "Account command timed out.", "account_io_failed": "Account state could not be saved.", "account_auth_failed": "Browser authentication failed.", "account_login_partial": "Credentials were saved, but native client activation failed.", "account_login_postcheck_partial": "Credentials were saved, but account state could not be verified.", "account_reference_empty": "Account reference must not be empty.", "account_not_found": "Account reference does not resolve.", "account_ambiguous": "Account reference is ambiguous; use an exact account key.", "account_unstable": "Account reference resolves to an unstable account.", "account_not_activatable": "Account has no managed credentials that can be activated.", "account_activation_partial": "Native client credentials changed, but account metadata needs recovery.", "account_confirmation_required": "Local credential removal requires --yes in non-interactive mode.", "account_read_only": "Account credentials are externally managed and cannot be removed by CQ.", "account_removal_partial": "Local credential removal is incomplete; recover the pending operation before retrying.", "interrupted": "Operation interrupted; inspect state before retrying.",
	}
	return cli.Outcome{ExitCode: exits[code], Errors: []cli.Diagnostic{{Code: code, Message: messages[code]}}}
}

func defaultV2AccountMutationDependencies(removing bool, diagnostic io.Writer) v2AccountMutationDependencies {
	return v2AccountMutationDependencies{
		Login: func(ctx context.Context, id provider.ID, activate bool) (app.AccountLoginResult, error) {
			client := httputil.NewClient(0, version)
			if id == provider.Claude {
				return app.LoginClaude(ctx, client, activate, func(ctx context.Context, http httputil.Doer) (*auth.TokenResponse, *auth.Profile, error) {
					return auth.LoginWithBrowser(ctx, http, func(ctx context.Context, url string) error { return auth.OpenBrowserContextTo(ctx, url, diagnostic) })
				}, &claudeprov.Accounts{HTTP: client})
			}
			control, err := codexprov.OpenDefaultCanonicalCredentialControl(ctx, fsutil.OSFileSystem{})
			if err != nil {
				return app.AccountLoginResult{}, err
			}
			defer control.Close()
			result, err := app.LoginCodex(ctx, client, activate, func(ctx context.Context, http httputil.Doer) (*auth.CodexTokenResponse, *auth.CodexClaims, error) {
				return auth.CodexLoginWithBrowser(ctx, http, func(ctx context.Context, url string) error { return auth.OpenBrowserContextTo(ctx, url, diagnostic) })
			}, control.CanonicalAdmin())
			err = errors.Join(err, v2LoginAliases(ctx, &result, &codexprov.Accounts{FS: fsutil.OSFileSystem{}}))
			return result, err
		},
		Select: func(ctx context.Context, id provider.ID, reference string) (v2AccountSelection, error) {
			if id == provider.Claude {
				accounts := &claudeprov.Accounts{HTTP: httputil.NewClient(0, version)}
				rows, err := accounts.Inspect(ctx)
				if err != nil {
					return v2AccountSelection{}, err
				}
				selected, err := claudeprov.ResolveAccountReference(rows, reference)
				if err != nil {
					return v2AccountSelection{}, err
				}
				return v2SelectClaudeMutation(selected, accounts), nil
			}
			roots, err := userdirs.Default(userdirs.StateRoot)
			if err != nil {
				return v2AccountSelection{}, err
			}
			accounts := &codexprov.Accounts{FS: fsutil.OSFileSystem{}, StateDir: roots.State}
			var inventory codexprov.Inventory
			var pending *codexprov.RemovalPlan
			if removing {
				inventory, pending, err = accounts.InspectRemoval(ctx)
			} else {
				inventory, err = accounts.Inspect(ctx)
			}
			if err != nil {
				return v2AccountSelection{}, err
			}

			aliases, err := accounts.InspectAliases(ctx)
			if err != nil {
				return v2AccountSelection{}, err
			}
			key, err := codexprov.ResolveAccountReference(inventory, aliases, reference)
			if err != nil {
				return v2AccountSelection{}, err
			}
			native, err := v2CodexNativeSnapshot(inventory)
			if err != nil {
				return v2AccountSelection{}, err
			}
			for i, logical := range inventory.Accounts {
				if logical.Key == key {
					operationID := ""
					if pending != nil {
						if pending.AccountKey != key {
							return v2AccountSelection{}, v2MutationDiagnostic("account_unstable")
						}
						operationID = pending.OperationID
					}
					return v2SelectCodexMutation(logical, v2CodexAccounts(inventory, aliases)[i], operationID, native, func(ctx context.Context) (*codexprov.CredentialControl, error) {
						return codexprov.OpenDefaultCanonicalCredentialControl(ctx, fsutil.OSFileSystem{})
					}), nil
				}
			}
			return v2AccountSelection{}, v2MutationDiagnostic("account_not_found")
		},
		Invalidate: func(ctx context.Context, id provider.ID) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			roots, err := userdirs.Default(userdirs.CacheRoot)
			if err != nil {
				return err
			}
			err = os.Remove(filepath.Join(roots.Cache, string(id)+".json"))
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		},
	}
}
func v2SelectClaudeMutation(selected keyring.ClaudeAccountInspection, accounts *claudeprov.Accounts) v2AccountSelection {
	row := v2ClaudeAccounts([]keyring.ClaudeAccountInspection{selected})[0]
	selection := v2AccountSelection{Account: row, Activatable: selected.Active, Retained: []string{}}
	for _, source := range row.Sources {
		selection.Activatable = selection.Activatable || source == "cq_managed"
		selection.Removable = selection.Removable || source != "external"
		if source == "external" {
			selection.Retained = append(selection.Retained, source)
		}
	}
	selection.Activate = func(ctx context.Context) (v2AccountActivation, error) {
		result, err := accounts.ActivateSelected(ctx, selected)
		return v2AccountActivation{Changed: result.Changed, Active: result.Active}, err
	}
	selection.Remove = func(ctx context.Context) (v2AccountRemoval, error) {
		result, err := accounts.RemoveSelected(ctx, selected.Account)
		return v2AccountRemoval{Changed: result.Changed, ActiveRemoved: result.ActiveRemoved}, err
	}
	return selection
}
func v2SelectCodexMutation(logical codexprov.LogicalAccount, row AccountSummary, operationID string, native codexprov.SystemSnapshot, open func(context.Context) (*codexprov.CredentialControl, error)) v2AccountSelection {
	selected := codexprov.ActivationSelection{AccountKey: logical.Key, WasActive: logical.Active}
	revisions := codexprov.RevisionSet{}
	selection := v2AccountSelection{Account: row, Activatable: logical.Active, Removable: operationID != "", Retained: []string{}}
	for _, candidate := range logical.Candidates {
		switch candidate.Source {
		case codexprov.SourceSystem:
			selected.ActiveRevision = candidate.Revision
			selection.Removable = true
		case codexprov.SourceManaged:
			selection.Removable = true
			revisions[candidate.Ref.CandidateID] = candidate.Revision
			if selected.Ref.CandidateID == "" && !candidate.DispatchBlocked && (candidate.AccessExpiresAt.IsZero() || candidate.AccessExpiresAt.After(time.Now())) {
				selected.Ref, selected.Revision = candidate.Ref, candidate.Revision
				selection.Activatable = true
			}
		case codexprov.SourceExternal:
			if len(selection.Retained) == 0 {
				selection.Retained = append(selection.Retained, "external")
			}
		}
	}
	selection.Activate = func(ctx context.Context) (v2AccountActivation, error) {
		control, err := open(ctx)
		if err != nil {
			return v2AccountActivation{}, err
		}
		defer control.Close()
		result, err := control.CanonicalAdmin().ActivateAccount(ctx, selected)
		return v2AccountActivation{Changed: result.Changed, Active: result.SystemCommitted}, errors.Join(err, result.ProjectionError)
	}
	selection.Remove = func(ctx context.Context) (v2AccountRemoval, error) {
		control, err := open(ctx)
		if err != nil {
			return v2AccountRemoval{}, err
		}
		defer control.Close()
		result, err := control.CanonicalAdmin().RemoveSelected(ctx, logical.Key, revisions, operationID, &native)
		return v2AccountRemoval{Changed: result.ManagedDeleted > 0 || result.SystemDeactivated || err == nil && operationID != "", ActiveRemoved: result.SystemDeactivated, Pending: result.PendingRecovery}, errors.Join(err, result.ProjectionError)
	}
	return selection
}

// Alias observation belongs to the same post-save budget as native state.
func v2LoginAliases(ctx context.Context, result *app.AccountLoginResult, accounts *codexprov.Accounts) error {
	if !result.CredentialsSaved || !result.AccountObserved {
		return nil
	}
	aliases, err := accounts.InspectAliases(ctx)
	if err != nil {
		result.AccountObserved = false
		return errors.Join(app.ErrAccountLoginPostcheck, err)
	}
	result.Aliases = aliases.Aliases(codexprov.AccountKey(result.Reference))
	return nil
}

func v2CodexNativeSnapshot(inventory codexprov.Inventory) (codexprov.SystemSnapshot, error) {
	native := codexprov.SystemSnapshot{}
	for _, account := range inventory.Accounts {
		for _, candidate := range account.Candidates {
			if candidate.Source != codexprov.SourceSystem {
				continue
			}
			if native.Present || account.Identity.RecordKey == "" || candidate.Revision == "" {
				return codexprov.SystemSnapshot{}, v2MutationDiagnostic("account_unstable")
			}
			native = codexprov.SystemSnapshot{Present: true, AccountKey: codexprov.AccountKey(account.Identity.RecordKey), Revision: candidate.Revision}
		}
	}
	return native, nil
}
