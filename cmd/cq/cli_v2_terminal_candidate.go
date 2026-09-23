package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func candidateDisplay(value string, role cli.Role) cli.Span {
	if value == "" {
		return cli.Span{Text: "—", Role: cli.Secondary}
	}
	return cli.Span{Text: value, Role: role}
}

func candidateOptional(value *string, role cli.Role) cli.Span {
	if value == nil {
		return candidateDisplay("", role)
	}
	return candidateDisplay(*value, role)
}

func candidateField(o cli.TerminalOptions, width int, label string, value cli.Span) string {
	return cli.TerminalLine(o, 2, cli.Span{Text: fmt.Sprintf("%-*s", width, label)}, value)
}

func terminalCandidateStatus(s v2CandidateStatus, o cli.TerminalOptions) string {
	phase := cli.Span{Text: string(s.Phase), Role: cli.Identity}
	if s.Phase == proxy.CandidatePhaseStopped || s.Phase == proxy.CandidatePhaseRemoved {
		phase.Role = cli.Confirmed
	}
	updated := cli.Span{Text: "unknown", Role: cli.Attention}
	if at, err := time.Parse(time.RFC3339Nano, s.UpdatedAt); err == nil {
		updated = cli.TerminalTimestamp(at, o)
		if !o.Now.IsZero() {
			relative := cli.TerminalTime(at, o, false).Text
			if at.Equal(o.Now) {
				relative = "0s ago"
			} else if i := strings.Index(relative, " · "); i >= 0 {
				relative = relative[:i]
			}
			updated.Text = relative + " · " + updated.Text
		}
	}
	pending := candidateOptional(s.PendingAction, cli.Attention)
	effect := cli.Span{Text: "no", Role: cli.Secondary}
	if s.EffectStarted {
		effect = cli.Span{Text: "yes", Role: cli.Attention}
	}
	active := candidateOptional(s.ActiveReleaseDigest, cli.Identity)
	if s.ActiveReleaseDigest == nil && (s.Phase == proxy.CandidatePhaseStopped || s.Phase == proxy.CandidatePhaseRemoved) {
		active = cli.Span{Text: "unknown", Role: cli.Attention}
	}
	text := cli.TerminalLine(o, 0, cli.Span{Text: "Candidate", Role: cli.Heading}, cli.Span{Text: " · "}, phase)
	for _, field := range []struct {
		label string
		value cli.Span
	}{
		{"Instance", candidateDisplay(s.InstanceID, cli.Identity)},
		{"State", candidateDisplay(s.StateDir, cli.Identity)},
		{"Port", cli.Span{Text: strconv.Itoa(s.Port), Role: cli.Identity}},
		{"Operation", candidateDisplay(s.OperationID, cli.Identity)},
		{"Validation run", candidateDisplay(s.ValidationRunID, cli.Identity)},
		{"Generation", cli.Span{Text: strconv.FormatUint(s.Generation, 10), Role: cli.Identity}},
		{"Updated", updated},
		{"Pending action", pending},
		{"Effect started", effect},
	} {
		text += candidateField(o, 16, field.label, field.value)
	}
	text += "\n" + cli.TerminalLine(o, 0, cli.Span{Text: "Identity and authority", Role: cli.Heading})
	for _, field := range []struct {
		label string
		value cli.Span
	}{
		{"Client build", candidateDisplay(s.ClientBuild, cli.Identity)},
		{"Credential mode", candidateDisplay(s.CredentialMode, cli.Identity)},
		{"Payload capture", cli.Span{Text: "no", Role: cli.Secondary}},
		{"Attempt", candidateOptional(s.AttemptID, cli.Identity)},
	} {
		if field.label == "Payload capture" && s.PayloadCapture {
			field.value = cli.Span{Text: "yes", Role: cli.Identity}
		}
		text += candidateField(o, 17, field.label, field.value)
	}
	text += "\n" + cli.TerminalLine(o, 0, cli.Span{Text: "Digests", Role: cli.Heading})
	for _, field := range []struct {
		label string
		value cli.Span
	}{
		{"Source config", candidateDisplay(s.SourceConfigDigest, cli.Identity)},
		{"Target bundle file", candidateDisplay(s.TargetBundleFileDigest, cli.Identity)},
		{"Target release", candidateDisplay(s.TargetReleaseDigest, cli.Identity)},
		{"Active release", active},
		{"Client executable", candidateDisplay(s.ClientExecutableDigest, cli.Identity)},
		{"Client registry file", candidateDisplay(s.ClientRegistryDigest, cli.Identity)},
		{"Credential manifest", candidateOptional(s.CredentialManifestDigest, cli.Identity)},
		{"Policy snapshot", candidateOptional(s.PolicySnapshotDigest, cli.Identity)},
		{"Effect receipt", candidateOptional(s.EffectReceiptDigest, cli.Identity)},
		{"Client safety receipt", candidateOptional(s.ClientSafetyReceiptDigest, cli.Identity)},
		{"Validation receipt", candidateOptional(s.ValidationReceiptDigest, cli.Identity)},
	} {
		text += candidateField(o, 23, field.label, field.value)
	}
	return text
}

func terminalCandidateReceipt(r v2CandidateReceipt, o cli.TerminalOptions) string {
	state := cli.Span{Text: r.Outcome, Role: cli.Confirmed}
	if r.Outcome == "conflicted" {
		state.Role = cli.Failed
	}
	text := cli.TerminalLine(o, 0, cli.Span{Text: "Candidate receipt", Role: cli.Heading}, cli.Span{Text: " · "}, state)
	for _, field := range []struct{ label, value string }{
		{"Attempt", r.AttemptID},
		{"Receipt digest", r.ReceiptDigest},
		{"Promotion digest", r.PromotionDigest},
	} {
		text += candidateField(o, 18, field.label, candidateDisplay(field.value, cli.Identity))
	}
	return text
}
