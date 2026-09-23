package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func routingLine(o cli.TerminalOptions, indent, labelWidth int, label string, value ...cli.Span) string {
	return cli.TerminalLine(o, indent, append([]cli.Span{{Text: fmt.Sprintf("%-*s", labelWidth, label)}}, value...)...)
}

func routingIdentity(value string) cli.Span { return cli.Span{Text: value, Role: cli.Identity} }
func routingQuantity(value uint64) cli.Span {
	return cli.Span{Text: fmt.Sprint(value), Role: cli.Quantity}
}
func routingAttention(value string) cli.Span { return cli.Span{Text: value, Role: cli.Attention} }

func terminalSelection(data v2SelectionData, action string, o cli.TerminalOptions) string {
	provider := "Codex"
	if data.Provider == "claude" {
		provider = "Claude"
	}
	kind := data.Kind
	if data.Provider == "claude" {
		kind = "proxy pin"
	}
	status := "not configured"
	if data.Configured {
		status = "configured"
	}
	if data.Provider == "claude" {
		status = "Not configured"
		if data.Configured {
			status = "Configured"
		}
		account := cli.Span{Text: "none"}
		if data.Account != nil {
			account = routingIdentity(*data.Account)
		}
		application := "Saved configuration only"
		if action != "show" {
			application = "Hot reload · asynchronous"
		}
		return cli.TerminalLine(o, 0, cli.Span{Text: provider + " " + kind, Role: cli.Heading}, cli.Span{Text: " · " + status, Role: cli.Identity}) +
			routingLine(o, 2, 17, "Account", account) +
			routingLine(o, 2, 17, "Application", cli.Span{Text: application, Role: cli.Secondary}) +
			routingLine(o, 2, 17, "Restart required", cli.Span{Text: "No"})
	}
	account := cli.Span{Text: "not configured", Role: cli.Attention}
	if data.Account != nil {
		account = routingIdentity(*data.Account)
	}
	text := ""
	if action != "show" {
		text = cli.TerminalLine(o, 0, cli.Span{Text: "Saved Codex " + kind + " configuration.", Role: cli.Confirmed})
	}
	text += cli.TerminalLine(o, 0, cli.Span{Text: provider + " " + kind, Role: cli.Heading}, cli.Span{Text: " · " + status, Role: cli.Identity})
	text += routingLine(o, 2, 13, "Account key", account)
	text += routingLine(o, 2, 13, "Source", cli.Span{Text: "Saved configuration", Role: cli.Secondary})
	text += routingLine(o, 2, 13, "Application", routingAttention("Live process acknowledgement not checked"))
	if action != "show" {
		text += "\n" + cli.TerminalLine(o, 0, cli.Span{Text: "Restart required: yes", Role: cli.Attention})
		text += cli.TerminalLine(o, 0, cli.Span{Text: "Next: "}, routingIdentity("cq service restart --component proxy"))
	}
	return text
}

func terminalPrime(enabled bool, overrides map[string]string, action string, o cli.TerminalOptions) string {
	status := "configured disabled"
	if enabled {
		status = "configured enabled"
	}
	text := ""
	if action != "status" {
		text = cli.TerminalLine(o, 0, cli.Span{Text: "Saved Codex priming configuration.", Role: cli.Confirmed})
	}
	text += cli.TerminalLine(o, 0, cli.Span{Text: "Codex priming", Role: cli.Heading}, cli.Span{Text: " · " + status, Role: cli.Identity})
	text += routingLine(o, 2, 13, "Application", routingAttention("Live process acknowledgement not checked")) + "\n"
	if len(overrides) == 0 {
		text += cli.TerminalLine(o, 0, cli.Span{Text: "Model overrides: none", Role: cli.Secondary})
	} else {
		text += cli.TerminalLine(o, 0, cli.Span{Text: "Model overrides", Role: cli.Heading})
		text += cli.TerminalLine(o, 2, cli.Span{Text: "Window       Model", Role: cli.Heading})
		windows := make([]string, 0, len(overrides))
		for window := range overrides {
			windows = append(windows, window)
		}
		sort.Strings(windows)
		for _, window := range windows {
			text += routingLine(o, 2, 13, window, routingIdentity(overrides[window]))
		}
	}
	text += cli.TerminalLine(o, 0, cli.Span{Text: "Source: saved configuration", Role: cli.Secondary})
	if action != "status" {
		text += "\n" + cli.TerminalLine(o, 0, cli.Span{Text: "Restart required: yes", Role: cli.Attention})
		text += cli.TerminalLine(o, 0, cli.Span{Text: "Next: "}, routingIdentity("cq service restart --component proxy"))
	}
	return text
}

func terminalLease(result proxy.CodexLeaseInvalidationResult, o cli.TerminalOptions) string {
	return cli.TerminalLine(o, 0, cli.Span{Text: "Codex leases invalidated", Role: cli.Heading}) +
		routingLine(o, 2, 20, "Invalidated leases", cli.TerminalCount(uint64(result.InvalidatedLeases))) +
		routingLine(o, 2, 20, "Journal generation", routingQuantity(result.JournalGeneration)) +
		"\n" + cli.TerminalLine(o, 0, cli.Span{Text: "Active authority and required account continuity remain intact."})
}

func routingParsedTime(value *string) (time.Time, bool) {
	if value == nil {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, *value)
	return parsed, err == nil
}

func routingReserveTime(value *string, o cli.TerminalOptions, deadline bool) cli.Span {
	if parsed, ok := routingParsedTime(value); ok {
		return cli.TerminalTime(parsed, o, deadline)
	}
	return routingAttention("unknown")
}

func routingReserveWindows(windows []v2ReserveWindow, o cli.TerminalOptions) string {
	if len(windows) == 0 {
		return cli.TerminalLine(o, 0, routingAttention("No system-account quota windows available."))
	}
	text := cli.TerminalLine(o, 0, cli.Span{Text: "System-account windows", Role: cli.Heading})
	for i, window := range windows {
		if i > 0 {
			text += "\n"
		}
		remaining := cli.TerminalPercent(float64(window.RemainingPct), false)
		if window.RemainingPctExact != nil {
			remaining = cli.TerminalPercent(*window.RemainingPctExact, false)
		}
		text += routingLine(o, 2, 11, "Window", routingIdentity(window.Selector))
		text += routingLine(o, 2, 11, "Remaining", remaining)
		text += routingLine(o, 2, 11, "Reset", routingReserveTime(window.ResetAt, o, true))
	}
	return text
}

func terminalReserve(data v2ReserveStatus, action string, o cli.TerminalOptions) string {
	if action == "windows" {
		return routingReserveWindows(data.Windows, o)
	}
	text := ""
	switch action {
	case "set":
		text = "Saved system-account reserve."
	case "enable":
		text = "Enabled system-account reserve protection."
	case "disable":
		text = "Temporarily bypassed system-account reserve."
	case "clear":
		text = "Removed system-account reserve configuration."
	}
	if text != "" {
		text = cli.TerminalLine(o, 0, cli.Span{Text: text, Role: cli.Confirmed})
	}
	state := "not configured"
	switch {
	case data.Configured && data.Reason != nil && *data.Reason == "disabled_until_reset":
		state = "temporarily bypassed"
	case data.Configured && data.Blocked:
		state = "excludes system account"
	case data.Configured && data.Enabled:
		state = "protection enabled"
	case data.Configured:
		state = "disabled"
	}
	stateRole := cli.Identity
	if data.Configured && data.Blocked {
		stateRole = cli.Attention
	}
	text += cli.TerminalLine(o, 0, cli.Span{Text: "Codex reserve", Role: cli.Heading}, cli.Span{Text: " · " + state, Role: stateRole})
	value := func(p *string, absent string) cli.Span {
		if p == nil {
			return routingAttention(absent)
		}
		return routingIdentity(*p)
	}
	remaining := routingAttention("unknown")
	if data.RemainingPct != nil {
		remaining = cli.TerminalPercent(*data.RemainingPct, false)
	}
	protected := routingAttention("not configured")
	if data.Percent != nil {
		protected = cli.TerminalPercent(*data.Percent, true)
		protected.Text += " of full window"
	}
	enabled := cli.Span{Text: "no"}
	if data.Enabled {
		enabled = cli.Span{Text: "yes", Role: cli.Confirmed}
	}
	routing := cli.Span{Text: "not excluded by reserve", Role: cli.Confirmed}
	if data.Blocked {
		routing = routingAttention("excluded by reserve")
	}
	reason := cli.Span{Text: "none", Role: cli.Secondary}
	if data.Reason != nil {
		switch *data.Reason {
		case "system_account_unavailable":
			reason = routingAttention("system account unavailable (system_account_unavailable)")
		case "reserve_state_write_failed":
			reason = routingAttention("reserve state could not be saved (reserve_state_write_failed)")
		case "usage_stale":
			reason = routingAttention("usage observation stale (usage_stale)")
		case "usage_unsettled":
			reason = routingAttention("usage observation unsettled (usage_unsettled)")
		case "reserve_reached":
			reason = routingAttention("protected threshold reached (reserve_reached)")
		case "disabled_until_reset":
			reason = routingAttention("bypassed until verified reset (disabled_until_reset)")
		default:
			reason = routingAttention(*data.Reason)
		}
	}
	fields := []struct {
		label string
		span  cli.Span
	}{
		{"Scope", cli.Span{Text: "Current system account only"}},
		{"Account", value(data.Email, "unavailable")}, {"Account key", value(data.AccountKey, "unavailable")},
		{"Window", value(data.Window, "not configured")}, {"Protected", protected},
		{"Remaining", remaining}, {"Enabled", enabled}, {"Routing", routing}, {"Reason", reason},
		{"Reset", routingReserveTime(data.ResetAt, o, true)}, {"Observed", routingReserveTime(data.ObservedAt, o, false)},
	}
	for _, field := range fields {
		text += routingLine(o, 2, 15, field.label, field.span)
	}
	return text + "\n" + routingReserveWindows(data.Windows, o)
}

func terminalPolicy(data v2PolicyDocument, kind, action string, offline bool, o cli.TerminalOptions) string {
	title := "Codex routing policy"
	if kind == "policy" && action == "apply" {
		title = "Saved Codex routing policy."
	}
	if kind == "pool" {
		title = "Saved Codex pool policy."
	}
	if kind == "session" {
		title = "Saved Codex session binding."
		if action == "unbind" {
			title = "Removed Codex session binding."
		}
	}
	text := cli.TerminalLine(o, 0, cli.Span{Text: title, Role: cli.Heading})
	text += routingLine(o, 2, 22, "Authority generation", routingQuantity(data.AuthorityGeneration))
	text += routingLine(o, 2, 22, "Routing generation", routingQuantity(data.RoutingGeneration))
	text += routingLine(o, 2, 22, "Effective generation", routingQuantity(data.EffectiveGeneration))
	text += routingLine(o, 2, 22, "Schema", routingQuantity(uint64(data.SchemaVersion)))
	text += "\n" + cli.TerminalLine(o, 0, cli.Span{Text: "Pools", Role: cli.Heading})
	if len(data.Pools) == 0 {
		text += cli.TerminalLine(o, 2, cli.Span{Text: "none", Role: cli.Secondary})
	} else {
		text += cli.TerminalLine(o, 2, cli.Span{Text: "Name  Value  Members", Role: cli.Heading})
		for _, pool := range data.Pools {
			members := []string{"none"}
			if len(pool.Members) > 0 {
				members = members[:0]
				for _, member := range pool.Members {
					members = append(members, string(member))
				}
			}
			text += cli.TerminalLine(o, 2, routingIdentity(fmt.Sprintf("%-4s", pool.Name)), cli.Span{Text: "  "}, routingQuantity(uint64(pool.Value)), cli.Span{Text: "     "}, routingIdentity(members[0]))
			for _, member := range members[1:] {
				text += cli.TerminalLine(o, 15, routingIdentity(member))
			}
		}
	}
	text += "\n" + cli.TerminalLine(o, 0, cli.Span{Text: "Session bindings", Role: cli.Heading})
	if len(data.SessionBindings) == 0 {
		text += cli.TerminalLine(o, 2, cli.Span{Text: "none", Role: cli.Secondary})
	} else {
		for _, binding := range data.SessionBindings {
			text += terminalSessionBinding(binding, false, o)
		}
	}
	text += "\n" + cli.TerminalLine(o, 0, cli.Span{Text: "Capability account evidence", Role: cli.Heading})
	if len(data.CapabilityEvidence) == 0 {
		text += cli.TerminalLine(o, 2, cli.Span{Text: "none", Role: cli.Secondary})
	} else {
		text += cli.TerminalLine(o, 2, cli.Span{Text: "Account key  State", Role: cli.Heading})
		for _, evidence := range data.CapabilityEvidence {
			role := cli.Identity
			if evidence.State == proxy.CapabilityUnknown {
				role = cli.Attention
			}
			text += cli.TerminalLine(o, 2, routingIdentity(fmt.Sprintf("%-11s", evidence.AccountKey)), cli.Span{Text: "  "}, cli.Span{Text: string(evidence.State), Role: role})
		}
	}
	text += "\n" + cli.TerminalLine(o, 0, cli.Span{Text: "Capability routing", Role: cli.Heading})
	pool := cli.Span{Text: "none", Role: cli.Secondary}
	if data.CapabilityPool != nil {
		pool = routingIdentity(*data.CapabilityPool)
	}
	poolLabelWidth := 12
	if len(data.CapabilityPredicates) > 0 {
		poolLabelWidth = 6
	}
	text += routingLine(o, 2, poolLabelWidth, "Pool", pool)
	if len(data.CapabilityPredicates) == 0 {
		text += routingLine(o, 2, 12, "Predicates", cli.Span{Text: "none", Role: cli.Secondary})
	} else {
		text += cli.TerminalLine(o, 2, cli.Span{Text: "Predicates", Role: cli.Heading})
		for _, predicate := range data.CapabilityPredicates {
			text += cli.TerminalLine(o, 4, cli.Span{Text: "Schema "}, routingQuantity(uint64(predicate.SchemaVersion)), cli.Span{Text: "; capability "}, routingIdentity(predicate.Capability))
			text += cli.TerminalLine(o, 4, cli.Span{Text: "Product surface "}, routingIdentity(predicate.ProductSurface), cli.Span{Text: "; access path "}, routingIdentity(predicate.AccessPath), cli.Span{Text: "; authentication mode "}, routingIdentity(predicate.AuthMode))
			text += cli.TerminalLine(o, 4, cli.Span{Text: "Requested model "}, routingIdentity(predicate.RequestedModel), cli.Span{Text: "; effective model "}, routingIdentity(predicate.EffectiveModel))
		}
	}
	if len(data.CapabilityRoutingEvidence) == 0 {
		text += routingLine(o, 2, 12, "Evidence", cli.Span{Text: "none", Role: cli.Secondary})
	} else {
		text += cli.TerminalLine(o, 2, cli.Span{Text: "Evidence", Role: cli.Heading})
		for _, evidence := range data.CapabilityRoutingEvidence {
			text += cli.TerminalLine(o, 4, cli.Span{Text: "Schema "}, routingQuantity(uint64(evidence.SchemaVersion)), cli.Span{Text: "; account "}, routingIdentity(string(evidence.AccountKey)))
			text += cli.TerminalLine(o, 4, cli.Span{Text: "Account HMAC "}, routingIdentity(evidence.AccountKeyHMAC))
			text += cli.TerminalLine(o, 4, cli.Span{Text: "Workspace "}, routingIdentity(evidence.Workspace))
			text += cli.TerminalLine(o, 4, cli.Span{Text: "Capability "}, routingIdentity(evidence.Capability), cli.Span{Text: "; product surface "}, routingIdentity(evidence.ProductSurface))
			text += cli.TerminalLine(o, 4, cli.Span{Text: "Access path "}, routingIdentity(evidence.AccessPath), cli.Span{Text: "; authentication mode "}, routingIdentity(evidence.AuthMode))
			text += cli.TerminalLine(o, 4, cli.Span{Text: "Requested model "}, routingIdentity(evidence.RequestedModel), cli.Span{Text: "; effective model "}, routingIdentity(evidence.EffectiveModel))
			auth := "no"
			if evidence.Authenticated {
				auth = "yes"
			}
			stateRole := cli.Identity
			if evidence.State == proxy.CapabilityEvidenceUnknown {
				stateRole = cli.Attention
			}
			text += cli.TerminalLine(o, 4, cli.Span{Text: "Source "}, routingIdentity(evidence.Source), cli.Span{Text: "; state "}, cli.Span{Text: string(evidence.State), Role: stateRole}, cli.Span{Text: "; authenticated " + auth})
			text += cli.TerminalLine(o, 4, cli.Span{Text: "Observed "}, routingEvidenceTime(evidence.ObservedAt, o))
			if evidence.ExpiresAt != nil {
				text += cli.TerminalLine(o, 4, cli.Span{Text: "Expires "}, cli.TerminalTime(*evidence.ExpiresAt, o, true))
			}
			text += cli.TerminalLine(o, 4, cli.Span{Text: "Routing generation "}, routingQuantity(evidence.RoutingGeneration))
		}
	}
	text += "\n" + cli.TerminalLine(o, 0, cli.Span{Text: "Delegations", Role: cli.Heading})
	if len(data.Delegations) == 0 {
		text += cli.TerminalLine(o, 2, cli.Span{Text: "none", Role: cli.Secondary})
	} else {
		for _, delegation := range data.Delegations {
			text += cli.TerminalLine(o, 2, cli.Span{Text: "Caller "}, routingIdentity(delegation.Caller))
			if len(delegation.Accounts) == 0 {
				text += cli.TerminalLine(o, 2, cli.Span{Text: "Accounts none"})
			}
			for i, account := range delegation.Accounts {
				if i == 0 {
					text += cli.TerminalLine(o, 2, cli.Span{Text: "Accounts "}, routingIdentity(string(account)))
				} else {
					text += cli.TerminalLine(o, 11, routingIdentity(string(account)))
				}
			}
			if !delegation.ExpiresAt.IsZero() {
				text += cli.TerminalLine(o, 2, cli.Span{Text: "Expires "}, cli.TerminalTime(delegation.ExpiresAt, o, true))
			}
		}
	}
	text += "\n"
	application := cli.Span{Text: "generations match", Role: cli.Confirmed}
	if data.RoutingGeneration != data.EffectiveGeneration {
		application = routingAttention("routing generation not yet effective")
	}
	text += cli.TerminalLine(o, 0, cli.Span{Text: "Application: "}, application)
	source := "authenticated proxy control"
	if offline {
		source = "offline policy state; live application not checked"
	}
	return text + cli.TerminalLine(o, 0, cli.Span{Text: "Source: "}, cli.Span{Text: source, Role: cli.Secondary})
}

func routingEvidenceTime(value time.Time, o cli.TerminalOptions) cli.Span {
	span := cli.TerminalTime(value, o, false)
	if value.Nanosecond() == 0 || o.Now.IsZero() || !value.Before(o.Now) {
		return span
	}
	delta := o.Now.Sub(value)
	if delta >= time.Hour || delta < time.Minute {
		return span
	}
	_, stamp, ok := strings.Cut(span.Text, " · ")
	if !ok {
		return span
	}
	seconds := int(delta / time.Second)
	span.Text = fmt.Sprintf("%dm %ds ago · %s", seconds/60, seconds%60, stamp)
	return span
}

func terminalSessionBinding(binding proxy.SessionBindingDocument, title bool, o cli.TerminalOptions) string {
	text := ""
	if title {
		text = cli.TerminalLine(o, 0, cli.Span{Text: "Codex session binding", Role: cli.Heading})
	}
	return text + routingLine(o, 2, 8, "Digest", routingIdentity(binding.SessionDigest)) + routingLine(o, 2, 8, "Pool", routingIdentity(binding.Pool))
}

func terminalSessionList(bindings []proxy.SessionBindingDocument, o cli.TerminalOptions) string {
	if len(bindings) == 0 {
		return cli.TerminalLine(o, 0, cli.Span{Text: "No Codex session bindings configured."})
	}
	text := cli.TerminalLine(o, 0, cli.Span{Text: "Codex session bindings", Role: cli.Heading})
	for _, binding := range bindings {
		text += terminalSessionBinding(binding, false, o)
	}
	return text
}

func terminalSessionDigest(digest string, o cli.TerminalOptions) string {
	return cli.TerminalLine(o, 0, cli.Span{Text: "Codex session digest", Role: cli.Heading}) + cli.TerminalLine(o, 2, routingIdentity(digest))
}
