package main

import (
	"fmt"
	"strings"

	"github.com/jacobcxdev/cq/internal/cli"
)

func terminalAccountValue(value *string) cli.Span {
	if value == nil {
		return cli.Span{Text: "unknown", Role: cli.Attention}
	}
	return cli.Span{Text: *value, Role: cli.Identity}
}
func terminalAccountDetails(a AccountSummary, o cli.TerminalOptions) string {
	active := cli.Span{Text: "Not active", Role: cli.Identity}
	if a.Active {
		active = cli.Span{Text: "Active", Role: cli.Confirmed}
	}
	stable := cli.Span{Text: "Unstable", Role: cli.Attention}
	if a.Stable {
		stable = cli.Span{Text: "Stable", Role: cli.Identity}
	}
	sources, aliases := "none", "none"
	if len(a.Sources) > 0 {
		sources = strings.Join(a.Sources, ", ")
	}
	if len(a.Aliases) > 0 {
		aliases = strings.Join(a.Aliases, ", ")
	}
	fields := []struct {
		label string
		value cli.Span
	}{
		{"Reference", terminalAccountValue(a.AccountReference)}, {"Account ID", terminalAccountValue(a.AccountID)},
		{"Email", terminalAccountValue(a.Email)}, {"Native default", active}, {"Identity", stable},
		{"Sources", cli.Span{Text: sources, Role: cli.Secondary}}, {"Aliases", cli.Span{Text: aliases, Role: cli.Secondary}},
		{"Tier", terminalAccountValue(a.RateLimitTier)},
	}
	// Label is independent metadata, not a substitute for a returned tier.
	if a.Label != nil {
		fields = append(fields, struct {
			label string
			value cli.Span
		}{"Label", terminalAccountValue(a.Label)})
	}
	var b strings.Builder
	for _, f := range fields {
		b.WriteString(cli.TerminalLine(o, 2, cli.Span{Text: fmt.Sprintf("%-16s", f.label)}, f.value))
	}
	return b.String()
}
func terminalAccounts(title string, accounts []AccountSummary, o cli.TerminalOptions) string {
	text := cli.TerminalLine(o, 0, cli.Span{Text: title + " accounts", Role: cli.Heading}, cli.Span{Text: " · "}, cli.TerminalCount(uint64(len(accounts))))
	if len(accounts) == 0 {
		return text + cli.TerminalLine(o, 2, cli.Span{Text: "No configured accounts."})
	}
	for _, a := range accounts {
		text += "\n" + cli.TerminalLine(o, 2, cli.Span{Text: a.DisplayName, Role: cli.Identity}) + terminalAccountDetails(a, o)
	}
	return text
}
func terminalGeminiAccount(account *AccountSummary, available bool, o cli.TerminalOptions) string {
	if !available {
		return ""
	}
	if account == nil {
		return cli.TerminalLine(o, 0, cli.Span{Text: "Gemini account", Role: cli.Heading}, cli.Span{Text: " · Not configured"}) + cli.TerminalLine(o, 2, cli.Span{Text: "Set up authentication in Antigravity."})
	}
	return cli.TerminalLine(o, 0, cli.Span{Text: "Gemini account", Role: cli.Heading}, cli.Span{Text: " · Configured", Role: cli.Identity}) + cli.TerminalLine(o, 2, cli.Span{Text: account.DisplayName, Role: cli.Identity}) + "\n" + terminalAccountDetails(*account, o) + "\n" + cli.TerminalLine(o, 0, cli.Span{Text: "Managed by Antigravity · credentials not validated by this command", Role: cli.Secondary})
}
func terminalAccountLogin(title string, account *AccountSummary, activated *bool, exit int, o cli.TerminalOptions) string {
	status, role := "Credentials saved", cli.Confirmed
	if exit != 0 {
		status, role = "Partial", cli.Attention
	}
	text := cli.TerminalLine(o, 0, cli.Span{Text: title + " login", Role: cli.Heading}, cli.Span{Text: " · "}, cli.Span{Text: status, Role: role})
	name := cli.Span{Text: "Unknown", Role: cli.Attention}
	if account != nil {
		name = cli.Span{Text: account.DisplayName, Role: cli.Identity}
	}
	active := cli.Span{Text: "Unknown", Role: cli.Attention}
	if activated != nil {
		active = cli.Span{Text: "Not selected", Role: cli.Identity}
		if *activated {
			active = cli.Span{Text: "Selected", Role: cli.Confirmed}
		}
	}
	if exit != 0 {
		return text + cli.TerminalFields(o, 2, []cli.Field{{Label: "Credentials", Value: []cli.Span{{Text: "Saved", Role: cli.Confirmed}}}, {Label: "Account", Value: []cli.Span{name}}, {Label: "Native default", Value: []cli.Span{active}}})
	}
	text += cli.TerminalLine(o, 2, name) + cli.TerminalFields(o, 2, []cli.Field{{Label: "Native default", Value: []cli.Span{active}}})
	if account != nil {
		text += terminalAccountDetails(*account, o)
	}
	if activated != nil && *activated {
		text += "\n" + cli.TerminalLine(o, 0, cli.Span{Text: "Existing clients may need reconnection."})
	}
	return text
}
func terminalAccountActivation(title string, account AccountSummary, result v2AccountActivation, exit int, o cli.TerminalOptions) string {
	status, role := "Already active", cli.Confirmed
	if result.Changed {
		status = "Changed"
	}
	if exit != 0 {
		status, role = "Partial", cli.Attention
	}
	text := cli.TerminalLine(o, 0, cli.Span{Text: title + " native client default", Role: cli.Heading}, cli.Span{Text: " · "}, cli.Span{Text: status, Role: role})
	if exit != 0 {
		return text + cli.TerminalLine(o, 2, cli.Span{Text: "Credentials changed", Role: cli.Confirmed}) + cli.TerminalLine(o, 2, cli.Span{Text: "Metadata verification: "}, cli.Span{Text: "incomplete", Role: cli.Attention})
	}
	text += cli.TerminalLine(o, 2, cli.Span{Text: account.DisplayName, Role: cli.Identity}) + terminalAccountDetails(account, o)
	if result.Changed {
		text += "\n" + cli.TerminalLine(o, 0, cli.Span{Text: "Existing clients may need reconnection."})
	}
	return text
}
func terminalAccountRemoval(title, reference string, result v2AccountRemoval, retained []string, confirmed bool, exit int, o cli.TerminalOptions) string {
	if !confirmed {
		return cli.TerminalLine(o, 0, cli.Span{Text: "Cancelled", Role: cli.Attention}, cli.Span{Text: " · no change requested"}) + cli.TerminalLine(o, 2, cli.Span{Text: "No credentials removed."})
	}
	status, role := "Not removed", cli.Body
	if result.Changed {
		status, role = "Removed", cli.Confirmed
	}
	if exit != 0 {
		status, role = "Partial", cli.Attention
	}
	native := cli.Span{Text: "Not removed"}
	if result.ActiveRemoved {
		native = cli.Span{Text: "Removed", Role: cli.Confirmed}
	}
	sources := "none"
	if len(retained) > 0 {
		sources = strings.Join(retained, ", ")
	}
	return cli.TerminalLine(o, 0, cli.Span{Text: title + " local credentials", Role: cli.Heading}, cli.Span{Text: " · "}, cli.Span{Text: status, Role: role}) + cli.TerminalLine(o, 2, cli.Span{Text: "Reference           "}, cli.Span{Text: reference, Role: cli.Identity}) + cli.TerminalLine(o, 2, cli.Span{Text: "Native credentials  "}, native) + cli.TerminalLine(o, 2, cli.Span{Text: "External sources    "}, cli.Span{Text: sources, Role: cli.Secondary})
}
func terminalAccountRemovalPrompt(title string, a AccountSummary, o cli.TerminalOptions) string {
	active := "No"
	if a.Active {
		active = "Yes"
	}
	return cli.TerminalLine(o, 0, cli.Span{Text: "Remove " + title + " local credentials", Role: cli.Heading}) + cli.TerminalFields(o, 2, []cli.Field{{Label: "Account", Value: []cli.Span{{Text: a.DisplayName, Role: cli.Identity}}}, {Label: "Reference", Value: []cli.Span{terminalAccountValue(a.AccountReference)}}, {Label: "Native active credentials will be removed", Value: []cli.Span{{Text: active, Role: cli.Identity}}}, {Label: "Upstream account", Value: []cli.Span{{Text: "Retained"}}}}) + "\n" + cli.TerminalLine(o, 0, cli.Span{Text: "Remove local credentials for "}, cli.Span{Text: a.DisplayName, Role: cli.Identity}, cli.Span{Text: "? [y/N]"})
}

func terminalAuthRefresh(works []authRefreshWork, total, exit int, o cli.TerminalOptions) string {
	observed := 0
	for _, w := range works {
		if len(w.errors) == 0 || len(w.result.Accounts) > 0 {
			observed++
		}
	}
	if exit != 0 && observed == 0 {
		return ""
	}
	title := []cli.Span{{Text: "Authentication refresh", Role: cli.Heading}}
	if exit != 0 {
		status, role := "Failed", cli.Failed
		if exit == 8 {
			status, role = "Partial", cli.Attention
		}
		if exit == 130 {
			status, role = "Interrupted", cli.Attention
		}
		title = append(title, cli.Span{Text: " · "}, cli.Span{Text: status, Role: role})
	}
	text := cli.TerminalLine(o, 0, title...)
	if exit != 130 {
		// Numerical columns are right aligned, matching their comparison headings.
		if o.Width >= 56 && observed == len(works) {
			text += cli.TerminalLine(o, 2, cli.Span{Text: "Provider  Refreshed  Unchanged  Need login  Failed", Role: cli.Heading})
			for _, w := range works {
				r := w.result
				name := terminalProviderName(r.Provider)
				text += cli.TerminalLine(o, 2, cli.Span{Text: fmt.Sprintf("%-9s", name), Role: cli.Identity}, cli.Span{Text: fmt.Sprintf("%10d %10d %11d %7d", r.Refreshed, r.Unchanged, r.ReauthRequired, r.Failed), Role: cli.Quantity})
			}
		} else {
			for _, w := range works {
				r := w.result
				fields := []cli.Field{{Label: "Provider", Value: []cli.Span{{Text: terminalProviderName(r.Provider), Role: cli.Identity}}}}
				for _, count := range []struct {
					label string
					value int
				}{{"Refreshed", r.Refreshed}, {"Unchanged", r.Unchanged}, {"Need login", r.ReauthRequired}, {"Failed", r.Failed}} {
					value := cli.TerminalCount(uint64(count.value))
					if len(w.errors) > 0 && len(r.Accounts) == 0 {
						value = cli.Span{Text: "unknown", Role: cli.Attention}
					}
					fields = append(fields, cli.Field{Label: count.label, Value: []cli.Span{value}})
				}
				text += cli.TerminalFields(o, 2, fields)
			}
		}
		text += "\n"
	}
	for _, w := range works {
		r := w.result
		for _, a := range r.Accounts {
			status, role := "Unknown", cli.Attention
			switch a.Status {
			case "refreshed":
				status, role = "Refreshed", cli.Confirmed
			case "unchanged":
				status, role = "Unchanged", cli.Body
			case "reauth_required":
				status = "Login required"
			case "failed":
				status, role = "Failed", cli.Failed
			}
			spans := []cli.Span{{Text: a.AccountLabel, Role: cli.Identity}, {Text: " · "}, {Text: status, Role: role}}
			if a.Status == "unchanged" {
				reason := ""
				reasonRole := cli.Secondary
				switch a.Reason {
				case "not_expiring":
					reason = "More than 30 minutes remaining"
					reasonRole = cli.TimeValue
				case "unknown_expiry":
					reason = "Expiry unknown"
					reasonRole = cli.Attention
				case "read_only_source":
					reason = "Read-only source"
				case "no_refresh_token":
					reason = "No refresh token"
				}
				if reason != "" {
					spans = append(spans, cli.Span{Text: " · "}, cli.Span{Text: reason, Role: reasonRole})
				}
			}
			text += cli.TerminalLine(o, 2, spans...)
			if a.AccountKey != nil {
				text += cli.TerminalFields(o, 4, []cli.Field{{Label: "Reference", Value: []cli.Span{terminalAccountValue(a.AccountKey)}}})
			}
			saved := cli.Span{Text: "Unchanged"}
			if a.CredentialsChanged {
				saved = cli.Span{Text: "Saved", Role: cli.Confirmed}
			}
			text += cli.TerminalFields(o, 4, []cli.Field{{Label: "Credentials", Value: []cli.Span{saved}}})
		}
	}
	text += "\n"
	if total > 0 {
		noun := " accounts"
		if total == 1 {
			noun = " account"
		}
		text += cli.TerminalLine(o, 0, cli.Span{Text: "Credentials saved for "}, cli.TerminalCount(uint64(total)), cli.Span{Text: noun})
	} else if exit == 0 {
		text += cli.TerminalLine(o, 0, cli.Span{Text: "No credentials needed refreshing."})
	} else {
		text += cli.TerminalLine(o, 0, cli.Span{Text: "No credential saves confirmed."})
	}
	if exit == 130 {
		text += cli.TerminalLine(o, 0, cli.Span{Text: "Remaining refresh outcomes: "}, cli.Span{Text: "unknown", Role: cli.Attention})
	}
	return text
}
func terminalProviderName(id string) string {
	switch id {
	case "claude":
		return "Claude"
	case "codex":
		return "Codex"
	case "gemini":
		return "Gemini"
	}
	return id
}
