package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func validationField(o cli.TerminalOptions, label string, labelWidth int, value cli.Span) string {
	if labelWidth == 0 {
		return cli.TerminalLine(o, 2, cli.Span{Text: label + "  "}, value)
	}
	return cli.TerminalLine(o, 2, cli.Span{Text: fmt.Sprintf("%-*s", labelWidth, label)}, value)
}

func validationID(value string) cli.Span { return cli.Span{Text: value, Role: cli.Identity} }
func validationUnknown() cli.Span        { return cli.Span{Text: "unknown", Role: cli.Attention} }
func validationLiteralNumber(n int) cli.Span {
	return cli.Span{Text: strconv.Itoa(n), Role: cli.Identity}
}
func validationTime(t time.Time, o cli.TerminalOptions) cli.Span {
	if t.IsZero() {
		return validationUnknown()
	}
	stamp := cli.TerminalTimestamp(t, o)
	if o.Now.IsZero() {
		return stamp
	}
	relative, _, _ := strings.Cut(cli.TerminalTime(t, o, false).Text, " · ")
	if relative == "0ms ago" {
		relative = "0s ago"
	}
	return cli.Span{Text: relative + " · " + stamp.Text, Role: cli.TimeValue}
}
func validationOptionalTime(value *string, o cli.TerminalOptions, absent string) cli.Span {
	if value == nil {
		return cli.Span{Text: absent, Role: cli.Attention}
	}
	t, err := time.Parse(time.RFC3339Nano, *value)
	if err != nil {
		return validationUnknown()
	}
	return validationTime(t, o)
}
func validationOptionalID(value string) cli.Span {
	if value == "" {
		return cli.Span{Text: "—", Role: cli.Secondary}
	}
	return validationID(value)
}

func terminalReadiness(marker proxy.CodexReadinessMarker, build string, o cli.TerminalOptions) string {
	text := cli.TerminalLine(o, 0, cli.Span{Text: "Codex HTTP readiness", Role: cli.Heading}, cli.Span{Text: " · current", Role: cli.Confirmed})
	text += validationField(o, "Codex build", 13, validationID(build))
	text += validationField(o, "Validated", 13, validationTime(marker.ValidatedAt, o))
	text += validationField(o, "Evidence", 13, cli.Span{Text: "Retained validation; no live health check", Role: cli.Attention})
	text += "\n" + cli.TerminalLine(o, 0, cli.Span{Text: "Validation identity", Role: cli.Heading})
	fields := []struct {
		label string
		value cli.Span
	}{
		{"Marker version", validationLiteralNumber(marker.Version)}, {"CQ build", validationID(marker.CQBuild)},
		{"Transport", validationID(string(marker.Transport))}, {"Parser schema", validationLiteralNumber(marker.ParserSchema)},
		{"Lease schema", validationLiteralNumber(marker.LeaseSchema)}, {"Semantics revision", validationID(marker.SemanticsRevision)},
		{"Client build", validationID(marker.ClientBuild)}, {"Retry budget", validationLiteralNumber(marker.RetryBudget)},
		{"Fixture hash", validationID(marker.FixtureHash)}, {"CQ executable SHA-256", validationOptionalID(marker.CQExecutableSHA256)},
		{"Client executable SHA-256", validationOptionalID(marker.ClientExecutableSHA256)}, {"Service kind", validationOptionalID(marker.ServiceKind)},
		{"Service identity SHA-256", validationOptionalID(marker.ServiceIdentitySHA256)},
		{"Installed result", cli.Span{Text: marker.InstalledResult, Role: cli.Confirmed}},
	}
	for _, field := range fields {
		text += validationField(o, field.label, 0, field.value)
	}
	text += cli.TerminalLine(o, 2, cli.Span{Text: "Completed gates", Role: cli.Heading})
	if len(marker.CompletedGates) == 0 {
		text += cli.TerminalLine(o, 4, cli.Span{Text: "No completed gates.", Role: cli.Attention})
	}
	for _, gate := range marker.CompletedGates {
		text += cli.TerminalLine(o, 4, validationID(gate))
	}
	text += validationField(o, "Marker validated", 18, cli.TerminalTimestamp(marker.ValidatedAt, o))
	return text
}

func terminalHTTPValidation(port int, o cli.TerminalOptions) string {
	return cli.TerminalLine(o, 0, cli.Span{Text: "HTTP validation", Role: cli.Heading}, cli.Span{Text: " · requested", Role: cli.Attention}) +
		validationField(o, "Candidate port", 16, validationLiteralNumber(port)) +
		validationField(o, "Completion", 16, cli.Span{Text: "not yet known", Role: cli.Attention}) +
		"\n" + cli.TerminalLine(o, 0, cli.Span{Text: "Request accepted; validation has not completed."}) +
		cli.TerminalLine(o, 0, cli.Span{Text: "Inspect retained readiness evidence before retrying."})
}

func terminalWebSocketValidation(marker proxy.CodexReadinessMarker, o cli.TerminalOptions) string {
	text := cli.TerminalLine(o, 0, cli.Span{Text: "WebSocket validation", Role: cli.Heading}, cli.Span{Text: " · passed", Role: cli.Confirmed})
	text += validationField(o, "Client build", 14, validationID(marker.ClientBuild))
	text += validationField(o, "Validated", 14, validationTime(marker.ValidatedAt, o))
	text += validationField(o, "CQ build", 14, validationID(marker.CQBuild))
	text += validationField(o, "Gates passed", 14, cli.TerminalCount(uint64(len(marker.CompletedGates))))
	text += "\n" + cli.TerminalLine(o, 0, cli.Span{Text: "Evidence", Role: cli.Heading})
	fields := []struct {
		label string
		value cli.Span
	}{
		{"Version", validationLiteralNumber(marker.Version)}, {"Transport", validationID(string(marker.Transport))},
		{"Parser schema", validationLiteralNumber(marker.ParserSchema)}, {"Lease schema", validationLiteralNumber(marker.LeaseSchema)},
		{"Semantics revision", validationID(marker.SemanticsRevision)}, {"Retry budget", validationLiteralNumber(marker.RetryBudget)},
		{"Fixture SHA-256", validationID(marker.FixtureHash)}, {"CQ executable", validationOptionalID(marker.CQExecutableSHA256)},
		{"Client executable", validationOptionalID(marker.ClientExecutableSHA256)}, {"Service identity", validationOptionalID(marker.ServiceIdentitySHA256)},
		{"Service kind", validationOptionalID(marker.ServiceKind)},
		{"Installed result", cli.Span{Text: marker.InstalledResult, Role: cli.Confirmed}},
	}
	for _, field := range fields {
		if field.label == "Fixture SHA-256" {
			text += validationField(o, field.label, 21, field.value)
		} else {
			text += validationField(o, field.label, 20, field.value)
		}
	}
	text += cli.TerminalLine(o, 2, cli.Span{Text: "Completed gates", Role: cli.Heading})
	if len(marker.CompletedGates) == 0 {
		text += cli.TerminalLine(o, 4, cli.Span{Text: "No completed gates.", Role: cli.Attention})
	}
	for _, gate := range marker.CompletedGates {
		text += cli.TerminalLine(o, 4, validationID(gate))
	}
	return text
}

func terminalFixture(path string, fixture proxy.SanitisedCodexFixture, o cli.TerminalOptions) string {
	boolValue := func(value bool) cli.Span {
		if value {
			return cli.Span{Text: "yes"}
		}
		return cli.Span{Text: "no"}
	}
	text := cli.TerminalLine(o, 0, cli.Span{Text: "Fixture", Role: cli.Heading}, cli.Span{Text: " · created", Role: cli.Confirmed})
	fields := []struct {
		label string
		value cli.Span
	}{
		{"File", validationID(path)}, {"Request kind", validationID(string(fixture.RequestKind))},
		{"Input size", cli.Span{Text: cli.TerminalCount(uint64(fixture.BodyBytes)).Text + " B", Role: cli.Quantity}},
		{"Converted", validationTime(fixture.CapturedAt, o)}, {"Metadata source", validationID(string(fixture.MetadataSource))},
		{"Session hint", validationID(fixture.SessionHint)}, {"Thread hint", validationID(fixture.ThreadHint)},
		{"Turn hint", validationID(fixture.TurnHint)}, {"Previous response", boolValue(fixture.HasPreviousID)},
		{"Encrypted state", boolValue(fixture.HasEncrypted)}, {"Body SHA-256", validationID(fixture.BodyHash)},
		{"Schema", validationLiteralNumber(fixture.SchemaVersion)},
	}
	for _, field := range fields {
		text += validationField(o, field.label, 19, field.value)
	}
	return text
}

func terminalCanary(state v2CanaryState, action string, o cli.TerminalOptions) string {
	status := "inactive"
	role := cli.Attention
	if state.Active {
		status = "active"
	}
	if !state.Active && state.Finalised {
		role = cli.Confirmed
	}
	text := cli.TerminalLine(o, 0, cli.Span{Text: "Codex canary", Role: cli.Heading}, cli.Span{Text: " · " + status, Role: role})
	text += validationField(o, "Run", 19, validationID(state.RunID))
	finalised := "no"
	if state.Finalised {
		finalised = "yes"
	}
	text += validationField(o, "Finalised", 19, cli.Span{Text: finalised, Role: role})
	start, err := time.Parse(time.RFC3339Nano, state.StartedAt)
	if err != nil {
		text += validationField(o, "Started", 19, validationUnknown())
	} else {
		text += validationField(o, "Started", 19, validationTime(start, o))
	}
	text += validationField(o, "Last observed day", 19, validationOptionalTime(state.LastObservedAt, o, "unknown"))
	text += validationField(o, "Ended", 19, validationOptionalTime(state.EndedAt, o, "—"))
	text += "\n" + cli.TerminalLine(o, 0, cli.Span{Text: "Observations", Role: cli.Heading})
	counts := []struct {
		label   string
		value   uint64
		failure bool
	}{
		{"Admitted turns", state.AdmittedTurns, false}, {"Keyed mismatches", state.KeyedMismatches, true},
		{"Protected-state changes", state.AutomaticHashChanges, true}, {"Secret leaks", state.SecretLeaks, true},
		{"Unexplained lifecycles", state.UnexplainedLifecycles, true}, {"Live-session repairs", state.LiveSessionRepairs, true},
		{"Protected-state failures", state.ProtectedStateFailures, true},
		{"Consecutive calendar days", uint64(max(0, state.ConsecutiveCalendarDays)), false},
	}
	for _, count := range counts {
		// A failure count remains a quantity; only the state or cause is red.
		width := 27
		if count.label == "Keyed mismatches" || count.label == "Unexplained lifecycles" {
			width = 28
		}
		text += validationField(o, count.label, width, cli.TerminalCount(count.value))
	}
	for _, count := range counts {
		if count.failure && count.value > 0 {
			text += cli.TerminalLine(o, 2, cli.Span{Text: "Failure observed", Role: cli.Failed})
			break
		}
	}
	text += "\n" + cli.TerminalLine(o, 0, cli.Span{Text: "Build binding", Role: cli.Heading})
	fields := []struct {
		label string
		value cli.Span
	}{
		{"CQ build", validationID(state.Tuple.CQBuild)}, {"Client build", validationID(state.Tuple.ClientBuild)},
		{"Parser schema", validationLiteralNumber(state.Tuple.ParserSchema)}, {"Lease schema", validationLiteralNumber(state.Tuple.LeaseSchema)},
		{"Semantics revision", validationID(state.Tuple.SemanticsRevision)}, {"Retry budget", validationLiteralNumber(state.Tuple.RetryBudget)},
		{"Fixture SHA-256", validationID(state.Tuple.FixtureHash)}, {"Readiness fingerprint", validationID(state.Tuple.ReadinessFingerprint)},
	}
	for _, field := range fields {
		text += validationField(o, field.label, 23, field.value)
	}
	text += "\n" + cli.TerminalLine(o, 0, cli.Span{Text: "Protected digests", Role: cli.Heading})
	if len(state.ProtectedDigests) == 0 {
		text += cli.TerminalLine(o, 2, cli.Span{Text: "No protected digests.", Role: cli.Attention})
	}
	for _, digest := range state.ProtectedDigests {
		text += validationField(o, string(digest.Kind), 18, validationID(digest.Digest))
	}
	text += "\n" + cli.TerminalLine(o, 0, cli.Span{Text: "Finalisation", Role: cli.Heading})
	if state.Finalisation == nil {
		text += cli.TerminalLine(o, 2, cli.Span{Text: "Not finalised.", Role: cli.Attention})
	} else {
		text += validationField(o, "Stop request digest", 23, validationID(state.Finalisation.StopRequestDigest))
		text += validationField(o, "Process binding digest", 23, validationID(state.Finalisation.ProcessBindingDigest))
		text += validationField(o, "Counters digest", 23, validationID(state.Finalisation.CountersDigest))
		text += validationField(o, "Active sessions", 23, cli.TerminalCount(state.Finalisation.ActiveSessions))
	}
	if action == "stop" && state.Active {
		text += "\n" + cli.TerminalLine(o, 0, cli.Span{Text: "Stop requested; drain completion has not been observed.", Role: cli.Attention})
	}
	return strings.TrimSuffix(text, "\n") + "\n"
}
