package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/quota"
)

func terminalResetLabel(email, reference *string) cli.Span {
	if email != nil {
		return terminalAccountValue(email)
	}
	if reference != nil {
		return terminalAccountValue(reference)
	}
	return cli.Span{Text: "Unknown account", Role: cli.Attention}
}
func terminalResetAligned(o cli.TerminalOptions, indent, width int, label string, values ...cli.Span) string {
	return cli.TerminalLine(o, indent, append([]cli.Span{{Text: fmt.Sprintf("%-*s", width, label)}}, values...)...)
}
func terminalResetInventory(rows []ResetAccountInventory, complete, selected bool, o cli.TerminalOptions) string {
	status := cli.Span{Text: "Incomplete", Role: cli.Attention}
	if complete {
		status = cli.Span{Text: "Complete", Role: cli.Confirmed}
	}
	text := cli.TerminalLine(o, 0, cli.Span{Text: "Codex reset credits", Role: cli.Heading}, cli.Span{Text: " · "}, status)
	if len(rows) == 0 {
		if !complete {
			return text + cli.TerminalLine(o, 2, cli.Span{Text: "Account inventory unavailable.", Role: cli.Attention})
		}
		return text + cli.TerminalLine(o, 2, cli.Span{Text: "No Codex accounts found."})
	}
	for i, row := range rows {
		indent := 4
		if selected {
			indent = 2
		} else if complete || i > 0 {
			text += "\n"
		}
		text += cli.TerminalLine(o, 2, terminalResetLabel(row.Email, row.AccountReference))
		count := cli.Span{Text: "unknown", Role: cli.Attention}
		if row.AvailableCount != nil {
			count = cli.TerminalCount(uint64(*row.AvailableCount))
			count.Text += " credits"
			if *row.AvailableCount == 1 {
				count.Text = strings.TrimSuffix(count.Text, "s")
			}
		}
		text += cli.TerminalFields(o, indent, []cli.Field{{Label: "Reference", Value: []cli.Span{terminalAccountValue(row.AccountReference)}}, {Label: "Account ID", Value: []cli.Span{terminalAccountValue(row.AccountID)}}, {Label: "Available", Value: []cli.Span{count}}})
		if len(row.Credits) == 0 && len(row.Errors) == 0 && row.AvailableCount != nil {
			emptyIndent := indent
			if complete {
				text += "\n"
				if selected {
					emptyIndent = 0
				}
			}
			text += cli.TerminalLine(o, emptyIndent, cli.Span{Text: "No reset credits."})
		}
		for _, credit := range row.Credits {
			text += "\n"
			state := cli.Span{Text: string(credit.Status), Role: cli.Identity}
			switch credit.Status {
			case "available":
				state.Role = cli.Confirmed
			case "expired":
				state.Role = cli.Attention
			}
			supported := "No"
			if credit.Supported {
				supported = "Yes"
			}
			expiry := cli.Span{Text: "No expiry", Role: cli.Secondary}
			if credit.ExpiresAt != nil {
				expiry = cli.TerminalTime(*credit.ExpiresAt, o, true)
			}
			fields := []cli.Field{{Label: "Credit", Value: []cli.Span{{Text: credit.ID, Role: cli.Identity}}}, {Label: "Type", Value: []cli.Span{{Text: string(credit.ResetType), Role: cli.Identity}}}, {Label: "Status", Value: []cli.Span{state}}, {Label: "Supported", Value: []cli.Span{{Text: supported, Role: cli.Identity}}}, {Label: "Granted", Value: []cli.Span{cli.TerminalTime(credit.GrantedAt, o, false)}}, {Label: "Expires", Value: []cli.Span{expiry}}}
			for _, field := range fields {
				text += terminalResetAligned(o, indent, 13, field.Label, field.Value...)
			}
			if credit.ExpiresAt != nil && !o.Now.IsZero() && !credit.ExpiresAt.After(o.Now) && credit.Status != "expired" {
				text += terminalResetAligned(o, indent, 13, "Eligibility", cli.Span{Text: "Expired", Role: cli.Attention})
			}
			if credit.Title != nil {
				text += cli.TerminalFields(o, indent, []cli.Field{{Label: "Title", Value: []cli.Span{{Text: *credit.Title}}}})
			}
			if credit.Description != nil {
				text += cli.TerminalFields(o, indent, []cli.Field{{Label: "Description", Value: []cli.Span{{Text: *credit.Description}}}})
			}
		}
		for _, err := range row.Errors {
			spans := []cli.Span{{Text: err.Code, Role: cli.Identity}, {Text: ": " + err.Message}}
			if err.EntryIndex != nil {
				spans = append(spans, cli.Span{Text: " · entry "}, cli.TerminalCount(uint64(*err.EntryIndex)))
			}
			text += cli.TerminalLine(o, indent, spans...)
		}
	}
	return text
}
func terminalResetConfidence(value string) cli.Span {
	switch value {
	case "":
		return cli.Span{Text: "unknown", Role: cli.Attention}
	case "high":
		return cli.Span{Text: "High", Role: cli.Identity}
	case "low":
		return cli.Span{Text: "Low", Role: cli.Attention}
	}
	return cli.Span{Text: value, Role: cli.Identity}
}
func terminalResetDuration(seconds int64) cli.Span {
	remaining := seconds
	var parts []string
	for _, unit := range []struct {
		seconds int64
		label   string
	}{{86400, "d"}, {3600, "h"}, {60, "m"}, {1, "s"}} {
		if remaining >= unit.seconds {
			parts = append(parts, fmt.Sprintf("%d%s", remaining/unit.seconds, unit.label))
			remaining %= unit.seconds
		}
	}
	if len(parts) == 0 {
		parts = []string{"0s"}
	}
	return cli.Span{Text: strings.Join(parts, " "), Role: cli.TimeValue}
}
func terminalResetOptionalTime(value *time.Time, fallback string, o cli.TerminalOptions) cli.Span {
	if value == nil {
		return cli.Span{Text: fallback, Role: cli.Secondary}
	}
	return cli.TerminalTime(*value, o, true)
}
func terminalResetSchedule(s ResetSchedule, o cli.TerminalOptions) string {
	title := cli.Span{Text: "Advisory", Role: cli.Identity}
	inputs := cli.Span{Text: "Complete", Role: cli.Confirmed}
	if !s.Complete {
		title = cli.Span{Text: "Incomplete", Role: cli.Attention}
		inputs = title
	}
	search := cli.Span{Text: "Bounded", Role: cli.Identity}
	if s.Exact {
		search.Text = "Exact"
	}
	horizon := cli.Span{Text: "unknown", Role: cli.Attention}
	if !s.Horizon.IsZero() {
		horizon = cli.TerminalTime(s.Horizon, o, true)
	}
	text := cli.TerminalLine(o, 0, cli.Span{Text: "Codex reset plan", Role: cli.Heading}, cli.Span{Text: " · "}, title)
	for _, f := range []cli.Field{{Label: "Inputs", Value: []cli.Span{inputs}}, {Label: "Search", Value: []cli.Span{search}}, {Label: "Confidence", Value: []cli.Span{terminalResetConfidence(string(s.Confidence))}}, {Label: "Generated", Value: []cli.Span{cli.TerminalTimestamp(s.GeneratedAt, o)}}, {Label: "Horizon", Value: []cli.Span{horizon}}} {
		text += terminalResetAligned(o, 2, 12, f.Label, f.Value...)
	}
	text += "\n"
	if !s.Complete {
		text += cli.TerminalLine(o, 0, cli.Span{Text: "Recommendation unavailable until blockers are resolved."})
	} else if len(s.Items) == 0 {
		text += cli.TerminalLine(o, 0, cli.Span{Text: "No reset use recommended."})
	}
	for i, item := range s.Items {
		if i > 0 {
			text += "\n"
		}
		status := cli.Span{Text: string(item.Status), Role: cli.Identity}
		switch item.Status {
		case "scheduled":
			status.Text = "Scheduled"
		case "due_now":
			status.Text = "Due now"
			status.Role = cli.Attention
		case "deferred":
			status.Text = "Deferred"
		case "not_yet_useful":
			status.Text = "Not yet useful"
		case "unsupported":
			status.Text = "Unsupported"
			status.Role = cli.Attention
		}
		text += cli.TerminalLine(o, 2, terminalResetLabel(item.AccountEmail, &item.AccountReference), cli.Span{Text: " · "}, status)
		var restored []cli.Span
		for i, window := range item.RestoredPct {
			if i > 0 {
				restored = append(restored, cli.Span{Text: " · "})
			}
			restored = append(restored, cli.Span{Text: string(window.Name) + " ", Role: cli.Identity}, cli.TerminalPercent(window.Percentage, false))
		}
		if len(restored) == 0 {
			restored = []cli.Span{{Text: "None", Role: cli.Secondary}}
		}
		reasons := make([]string, len(item.ReasonCodes))
		for i, reason := range item.ReasonCodes {
			reasons[i] = string(reason)
		}
		reasonSpan := cli.Span{Text: strings.Join(reasons, ", "), Role: cli.Identity}
		if len(reasons) == 0 {
			reasonSpan = cli.Span{Text: "None", Role: cli.Secondary}
		}
		useAt := item.UseAt
		if !s.Complete {
			useAt = nil
		}
		fields := []cli.Field{{Label: "Reference", Value: []cli.Span{{Text: item.AccountReference, Role: cli.Identity}}}, {Label: "Account ID", Value: []cli.Span{terminalAccountValue(item.AccountID)}}, {Label: "Credit", Value: []cli.Span{{Text: item.CreditID, Role: cli.Identity}}}, {Label: "Use at", Value: []cli.Span{terminalResetOptionalTime(useAt, "Not scheduled", o)}}, {Label: "Use by", Value: []cli.Span{terminalResetOptionalTime(item.UseBy, "No expiry", o)}}, {Label: "Confidence", Value: []cli.Span{terminalResetConfidence(string(item.Confidence))}}, {Label: "Gap avoided", Value: []cli.Span{terminalResetDuration(item.AvoidedGapSeconds)}}, {Label: "Restored", Value: restored}, {Label: "Reasons", Value: []cli.Span{reasonSpan}}}
		for _, f := range fields {
			text += terminalResetAligned(o, 4, 13, f.Label, f.Value...)
		}
	}
	text += "\n" + cli.TerminalLine(o, 0, cli.Span{Text: "Objective", Role: cli.Heading})
	if !s.ObjectiveComputed {
		text += cli.TerminalLine(o, 2, cli.Span{Text: "Objective unavailable.", Role: cli.Attention})
	} else {
		unmet := cli.Span{Text: fmt.Sprintf("%g", s.Objective.UnmetDemandPctSeconds), Role: cli.Quantity}
		for _, f := range []cli.Field{{Label: "Unmet demand", Value: []cli.Span{unmet, {Text: " %-seconds"}}}, {Label: "Gap duration", Value: []cli.Span{terminalResetDuration(s.Objective.GapDurationSeconds)}}, {Label: "Useful credits expired unused", Value: []cli.Span{cli.TerminalCount(uint64(s.Objective.UsefulExpiredUnused))}}, {Label: "Weighted discarded quota", Value: []cli.Span{cli.TerminalPercent(s.Objective.WeightedDiscardedPct, false)}}, {Label: "Restored quota", Value: []cli.Span{cli.TerminalPercent(s.Objective.RestoredPct, false)}}} {
			text += terminalResetAligned(o, 2, 31, f.Label, f.Value...)
		}
	}
	text += "\n" + cli.TerminalLine(o, 0, cli.Span{Text: "Blockers", Role: cli.Heading})
	if len(s.Blockers) == 0 {
		text += cli.TerminalLine(o, 2, cli.Span{Text: "None", Role: cli.Secondary})
	}
	for _, b := range s.Blockers {
		text += cli.TerminalLine(o, 2, cli.Span{Text: b.Code, Role: cli.Identity}, cli.Span{Text: " · "}, terminalResetLabel(b.AccountEmail, b.AccountReference)) + cli.TerminalFields(o, 4, []cli.Field{{Label: "Reference", Value: []cli.Span{terminalAccountValue(b.AccountReference)}}, {Label: "Account ID", Value: []cli.Span{terminalAccountValue(b.AccountID)}}})
	}
	return text
}
func terminalResetUse(data ResetUseResult, exit int, o cli.TerminalOptions) string {
	credit := cli.Span{Text: "Not selected", Role: cli.Secondary}
	if data.CreditID != nil {
		credit = terminalAccountValue(data.CreditID)
	}
	if data.Outcome == "cancelled" {
		text := cli.TerminalLine(o, 0, cli.Span{Text: "Cancelled", Role: cli.Attention}, cli.Span{Text: " · no change requested"})
		consumption := cli.TerminalLine(o, 2, cli.Span{Text: "No credit consumed."})
		if exit != 0 {
			text = cli.TerminalLine(o, 0, cli.Span{Text: "Codex reset", Role: cli.Heading}, cli.Span{Text: " · "}, cli.Span{Text: "Not completed", Role: cli.Attention})
			consumption = cli.TerminalFields(o, 2, []cli.Field{{Label: "Consumption", Value: []cli.Span{{Text: "not confirmed", Role: cli.Attention}}}})
		}
		return text + cli.TerminalFields(o, 2, []cli.Field{{Label: "Account", Value: []cli.Span{{Text: data.AccountReference, Role: cli.Identity}}}, {Label: "Credit", Value: []cli.Span{credit}}}) + consumption
	}
	status := cli.Span{Text: "Completion unknown", Role: cli.Attention}
	switch data.Outcome {
	case "reset":
		status = cli.Span{Text: "Completed", Role: cli.Confirmed}
	case "already_redeemed":
		status = cli.Span{Text: "Already completed", Role: cli.Confirmed}
	case "nothing_to_reset":
		status = cli.Span{Text: "Nothing to reset"}
	case "no_credit":
		status = cli.Span{Text: "No eligible reset credit"}
	}
	spans := []cli.Span{{Text: "Codex reset", Role: cli.Heading}, {Text: " · "}, status}
	known := data.Outcome == "reset" || data.Outcome == "already_redeemed" || data.Outcome == "nothing_to_reset" || data.Outcome == "no_credit"
	if known && exit != 0 {
		spans = append(spans, cli.Span{Text: " · "}, cli.Span{Text: "Verification incomplete", Role: cli.Attention})
	}
	text := cli.TerminalLine(o, 0, spans...)
	count := cli.Span{Text: "unknown", Role: cli.Attention}
	if known {
		count = cli.TerminalCount(uint64(data.WindowsReset))
	}
	for _, f := range []cli.Field{{Label: "Account", Value: []cli.Span{{Text: data.AccountReference, Role: cli.Identity}}}, {Label: "Credit", Value: []cli.Span{credit}}, {Label: "Windows reset", Value: []cli.Span{count}}} {
		text += terminalResetAligned(o, 2, 15, f.Label, f.Value...)
	}
	if known && exit != 0 && len(data.ChangedWindows) == 0 {
		text += terminalResetAligned(o, 2, 16, "Window changes", cli.Span{Text: "Not verified", Role: cli.Attention})
	}
	for _, w := range data.ChangedWindows {
		text += "\n" + cli.TerminalLine(o, 2, cli.Span{Text: string(w.Name), Role: cli.Identity})
		for _, f := range []cli.Field{{Label: "Remaining before", Value: []cli.Span{cli.TerminalPercent(w.Before.RemainingPct, false)}}, {Label: "Remaining after", Value: []cli.Span{cli.TerminalPercent(w.After.RemainingPct, false)}}, {Label: "Reset before", Value: []cli.Span{cli.TerminalTime(w.Before.ResetAt, o, true)}}, {Label: "Reset after", Value: []cli.Span{cli.TerminalTime(w.After.ResetAt, o, true)}}, {Label: "Period before", Value: []cli.Span{terminalResetDuration(w.Before.PeriodSeconds)}}, {Label: "Period after", Value: []cli.Span{terminalResetDuration(w.After.PeriodSeconds)}}} {
			text += terminalResetAligned(o, 4, 18, f.Label, f.Value...)
		}
	}
	if data.Retry != nil {
		text += "\n" + cli.TerminalLine(o, 0, cli.Span{Text: "Retry only this request", Role: cli.Heading}) + cli.TerminalLine(o, 2, cli.Span{Text: "cq codex reset use " + terminalResetQuote(data.Retry.AccountReference) + " --credit " + terminalResetQuote(data.Retry.CreditID), Role: cli.Identity})
	}
	if data.Outcome == "already_redeemed" {
		text += "\n" + cli.TerminalLine(o, 0, cli.Span{Text: "This reset request was already completed."})
	}
	return text
}
func terminalResetQuote(value string) string {
	if value != "" && !strings.ContainsAny(value, " \t\r\n'\"\\$`;&|<>()[]{}*?!#~") {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
func terminalResetPreview(plan app.CodexResetUsePlan, prompt bool, o cli.TerminalOptions) string {
	name := plan.Email
	if name == "" {
		name = plan.AccountID
	}
	if name == "" {
		name = "Unknown account"
	}
	text := cli.TerminalLine(o, 0, cli.Span{Text: "Codex reset preview", Role: cli.Heading})
	for _, f := range []cli.Field{{Label: "Account", Value: []cli.Span{{Text: name, Role: cli.Identity}}}, {Label: "Reference", Value: []cli.Span{{Text: string(plan.AccountKey), Role: cli.Identity}}}, {Label: "Credit", Value: []cli.Span{{Text: plan.Credit.ID, Role: cli.Identity}}}} {
		text += terminalResetAligned(o, 2, 11, f.Label, f.Value...)
	}
	for _, name := range []quota.WindowName{quota.Window5Hour, quota.Window7Day} {
		window := v2ResetWindow(name, plan.CurrentWindows[name])
		text += terminalResetAligned(o, 2, 11, string(name), cli.TerminalPercent(window.RemainingPct, false), cli.Span{Text: " remaining · "}, cli.TerminalTime(window.ResetAt, o, true))
	}
	if plan.Recommendation != nil {
		status := "unavailable"
		for _, item := range plan.Recommendation.Items {
			if item.AccountReference == string(plan.AccountKey) && item.CreditID == plan.Credit.ID {
				status = string(item.Status)
				break
			}
		}
		text += cli.TerminalFields(o, 2, []cli.Field{{Label: "Recommendation", Value: []cli.Span{{Text: status, Role: cli.Identity}}}})
	}
	if prompt {
		text += "\n" + cli.TerminalLine(o, 0, cli.Span{Text: "Use reset credit "}, cli.Span{Text: plan.Credit.ID, Role: cli.Identity}, cli.Span{Text: " for "}, cli.Span{Text: name, Role: cli.Identity}, cli.Span{Text: "? [y/N]"})
	}
	return text
}
