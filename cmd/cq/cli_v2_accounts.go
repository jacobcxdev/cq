package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/keyring"
	"github.com/jacobcxdev/cq/internal/provider"
	claudeprov "github.com/jacobcxdev/cq/internal/provider/claude"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	geminiprov "github.com/jacobcxdev/cq/internal/provider/gemini"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

// AccountSummary is the complete non-secret CLI-v2 account resource. A native
// default is not a proxy pin, and stable identity does not confer write authority.
type AccountSummary struct {
	Provider         provider.ID `json:"provider"`
	AccountReference *string     `json:"account_reference"`
	AccountID        *string     `json:"account_id"`
	Email            *string     `json:"email"`
	DisplayName      string      `json:"display_name"`
	Label            *string     `json:"label"`
	RateLimitTier    *string     `json:"rate_limit_tier"`
	Active           bool        `json:"active"`
	Sources          []string    `json:"sources"`
	Aliases          []string    `json:"aliases"`
	Stable           bool        `json:"stable"`
}

type v2AccountDependencies struct {
	Codex  func(context.Context) (codexprov.Inventory, codexprov.AccountAliasIndex, error)
	Claude func(context.Context) ([]keyring.ClaudeAccountInspection, error)
	Gemini func(context.Context) ([]provider.Account, error)
}

// T27 installs this lookup into the executable's canonical dispatcher.
func lookupV2AccountInspection(path string) (cli.Handler, bool) {
	switch path {
	case "claude account list", "codex account list", "gemini account show":
		return handleV2AccountInspection, true
	default:
		return nil, false
	}
}

func handleV2AccountInspection(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome {
	return handleV2AccountInspectionWithDependencies(ctx, inv, session, v2AccountDependencies{
		Codex: func(ctx context.Context) (codexprov.Inventory, codexprov.AccountAliasIndex, error) {
			roots, err := userdirs.Default(userdirs.StateRoot)
			if err != nil {
				return codexprov.Inventory{}, codexprov.AccountAliasIndex{}, err
			}
			accounts := &codexprov.Accounts{FS: fsutil.OSFileSystem{}, StateDir: roots.State}
			inventory, err := accounts.Inspect(ctx)
			if err != nil {
				return inventory, codexprov.AccountAliasIndex{}, err
			}
			aliases, err := accounts.InspectAliases(ctx)
			return inventory, aliases, err
		},
		Claude: (&claudeprov.Accounts{}).Inspect,
		Gemini: func(ctx context.Context) ([]provider.Account, error) {
			return geminiprov.New(nil, "").DiscoverAccounts(ctx)
		},
	})
}

func handleV2AccountInspectionWithDependencies(parent context.Context, inv cli.Invocation, _ *cli.Session, deps v2AccountDependencies) cli.Outcome {
	timeout, err := time.ParseDuration(inv.Options["timeout"][0])
	if err != nil {
		return v2AccountFailure(inv.Path, false)
	}
	budget := cli.BeginBudget(parent, timeout, 0)
	defer budget.Close()
	ctx := budget.Work()
	type observation struct {
		accounts []AccountSummary
		err      error
	}
	done := make(chan observation, 1)
	if ctx.Err() == nil {
		go func() {
			result := observation{accounts: []AccountSummary{}}
			defer func() {
				if recover() != nil {
					result.err = errors.New("account inspection failed")
				}
				done <- result
			}()
			if err := ctx.Err(); err != nil {
				result.err = err
				return
			}
			switch inv.Path {
			case "codex account list":
				inventory, aliases, err := deps.Codex(ctx)
				result.accounts, result.err = v2CodexAccounts(inventory, aliases), err
			case "claude account list":
				accounts, err := deps.Claude(ctx)
				result.accounts, result.err = v2ClaudeAccounts(accounts), err
			case "gemini account show":
				accounts, err := deps.Gemini(ctx)
				result.err = err
				if len(accounts) > 1 {
					result.err = errors.New("invalid Gemini inventory")
				}
				for _, account := range accounts {
					row := v2AccountSummary(provider.Gemini, account.AccountID, account.Email, "", "")
					row.DisplayName = "Antigravity CLI"
					if row.Email != nil {
						row.DisplayName = *row.Email
					}
					row.Active, row.Stable = true, true
					row.Sources = []string{"external"}
					result.accounts = append(result.accounts, row)
				}
			default:
				result.err = errors.New("unknown account inspection")
			}
		}()
	}
	result := observation{accounts: []AccountSummary{}}
	select {
	case result = <-done:
	case <-ctx.Done():
	}
	outcome := cli.Outcome{}
	switch {
	case parent.Err() == context.Canceled || ctx.Err() == context.Canceled:
		outcome = cli.Outcome{ExitCode: 130, Errors: []cli.Diagnostic{{Code: "interrupted", Message: "Operation interrupted; inspect state before retrying."}}}
	case ctx.Err() == context.DeadlineExceeded || errors.Is(result.err, context.DeadlineExceeded):
		outcome = v2AccountFailure(inv.Path, true)
	case result.err != nil:
		var environment *userdirs.EnvironmentError
		if errors.As(result.err, &environment) {
			return cli.Outcome{ExitCode: environment.ExitCode, Errors: []cli.Diagnostic{{Code: environment.Code, Message: environment.Error()}}}
		}
		outcome = v2AccountFailure(inv.Path, false)
	}
	sort.SliceStable(result.accounts, func(i, j int) bool {
		a, b := result.accounts[i], result.accounts[j]
		if a.AccountReference == nil && b.AccountReference != nil {
			return false
		}
		if a.AccountReference != nil && b.AccountReference == nil {
			return true
		}
		if a.AccountReference != nil && b.AccountReference != nil && *a.AccountReference != *b.AccountReference {
			return *a.AccountReference < *b.AccountReference
		}
		return a.DisplayName < b.DisplayName
	})
	if inv.Path == "gemini account show" {
		var account *AccountSummary
		name := "none"
		if len(result.accounts) == 1 {
			account = &result.accounts[0]
			name = cli.HumanValue(account.DisplayName)
		}
		outcome.Data, err = cli.EncodeData(struct {
			Configured bool            `json:"configured"`
			Account    *AccountSummary `json:"account"`
		}{account != nil, account})
		outcome.Human = fmt.Sprintf("Gemini configured: %t.\nAccount: %s.\nManaged by: Antigravity.\n", account != nil, name)
	} else {
		outcome.Data, err = cli.EncodeData(struct {
			Accounts []AccountSummary `json:"accounts"`
		}{result.accounts})
		var human strings.Builder
		fmt.Fprintf(&human, "Accounts: %d\n", len(result.accounts))
		for _, account := range result.accounts {
			reference := "—"
			if account.AccountReference != nil {
				reference = cli.HumanValue(*account.AccountReference)
			}
			fmt.Fprintf(&human, "%s\t%s\tactive=%t\t%s\n", reference, cli.HumanValue(account.DisplayName), account.Active, strings.Join(account.Sources, ","))
		}
		outcome.Human = human.String()
	}
	if err != nil {
		return v2AccountFailure(inv.Path, false)
	}
	return outcome
}

func v2AccountFailure(path string, timeout bool) cli.Outcome {
	code, message, exit := "account_inventory_unavailable", "Account inventory is unavailable.", 4
	if timeout {
		code, message, exit = "account_timeout", "Account command timed out.", 7
	}
	if path == "gemini account show" {
		code, message = "gemini_account_unavailable", "Antigravity account configuration could not be read."
		if timeout {
			code, message = "gemini_account_timeout", "Antigravity account inspection timed out."
		}
	}
	return cli.Outcome{ExitCode: exit, Errors: []cli.Diagnostic{{Code: code, Message: message}}}
}

func v2AccountString(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return &value
}
func v2AccountSummary(id provider.ID, accountID, email, label, tier string) AccountSummary {
	row := AccountSummary{Provider: id, AccountID: v2AccountString(accountID), Email: v2AccountString(email), Label: v2AccountString(label), RateLimitTier: v2AccountString(tier), Sources: []string{}, Aliases: []string{}, DisplayName: "Unknown account"}
	for _, value := range []*string{row.Email, row.Label, row.AccountID} {
		if value != nil {
			row.DisplayName = *value
			break
		}
	}
	return row
}
func v2CodexAccounts(inventory codexprov.Inventory, aliases codexprov.AccountAliasIndex) []AccountSummary {
	rows := make([]AccountSummary, 0, len(inventory.Accounts))
	for _, account := range inventory.Accounts {
		row := v2AccountSummary(provider.Codex, account.Identity.AccountID, account.Identity.Email, account.Identity.PlanType, "")
		row.Active = account.Active
		if key, err := codexprov.ResolveAccountReference(inventory, aliases, string(account.Key)); err == nil {
			row.AccountReference = v2AccountString(string(key))
			row.Stable = row.AccountReference != nil
		}
		row.Aliases = aliases.Aliases(account.Key)
		sources := map[string]bool{}
		for _, candidate := range account.Candidates {
			switch candidate.Source {
			case codexprov.SourceSystem:
				sources["native_client"] = true
			case codexprov.SourceManaged:
				sources["cq_managed"] = true
			case codexprov.SourceExternal:
				sources["external"] = true
			}
		}
		for source := range sources {
			row.Sources = append(row.Sources, source)
		}
		sort.Strings(row.Sources)
		rows = append(rows, row)
	}
	return rows
}
func v2ClaudeAccounts(accounts []keyring.ClaudeAccountInspection) []AccountSummary {
	rows := make([]AccountSummary, 0, len(accounts))
	for _, account := range accounts {
		a := account.Account
		row := v2AccountSummary(provider.Claude, a.AccountUUID, a.Email, a.SubscriptionType, a.RateLimitTier)
		reference := strings.TrimSpace(a.Email)
		count := 0
		for _, other := range accounts {
			if strings.EqualFold(reference, strings.TrimSpace(other.Account.Email)) {
				count++
			}
		}
		row.Stable = a.AccountUUID != "" || reference != "" && count == 1
		if reference != "" && count == 1 {
			row.AccountReference = &reference
		}
		row.Active = account.Active
		row.Sources = append(row.Sources, account.Sources...)
		sort.Strings(row.Sources)
		rows = append(rows, row)
	}
	return rows
}
