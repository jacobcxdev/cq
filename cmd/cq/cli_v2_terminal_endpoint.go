package main

import (
	"strings"

	"github.com/jacobcxdev/cq/internal/cli"
	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
)

func terminalEndpointState(state string) cli.Span {
	switch codexprov.CredentialEndpointMaintenanceState(state) {
	case codexprov.CredentialEndpointMaintenanceActivated,
		codexprov.CredentialEndpointMaintenanceRolledBack,
		codexprov.CredentialEndpointMaintenanceCommitted:
		return cli.Span{Text: state, Role: cli.Confirmed}
	case codexprov.CredentialEndpointMaintenancePrepared,
		codexprov.CredentialEndpointMaintenanceQuarantined,
		codexprov.CredentialEndpointMaintenanceActivating,
		codexprov.CredentialEndpointMaintenanceFinalising,
		codexprov.CredentialEndpointMaintenanceCommitting,
		codexprov.CredentialEndpointMaintenanceRollingBack:
		return cli.Span{Text: state, Role: cli.Attention}
	default:
		return cli.Span{Text: state, Role: cli.Attention}
	}
}

func terminalEndpointPrompt(action, proofFile, path, ticketID string, o cli.TerminalOptions) string {
	verb := action
	if len(verb) > 0 {
		verb = strings.ToUpper(verb[:1]) + verb[1:]
	}
	title := verb + " credential endpoint migration"
	if action == "finalise" {
		title = "Finalise credential endpoint replacement"
	}
	var out strings.Builder
	out.WriteString(cli.TerminalLine(o, 0, cli.Span{Text: title, Role: cli.Heading}))
	if path != "" && ticketID != "" {
		out.WriteString(cli.TerminalLine(o, 2, cli.Span{Text: "Path    "}, cli.Span{Text: path, Role: cli.Identity}))
		out.WriteString(cli.TerminalLine(o, 2, cli.Span{Text: "Ticket  "}, cli.Span{Text: ticketID, Role: cli.Identity}))
	} else {
		out.WriteString(cli.TerminalLine(o, 2, cli.Span{Text: "Proof   "}, cli.Span{Text: proofFile, Role: cli.Identity}))
	}
	if action == "finalise" {
		out.WriteString(cli.TerminalLine(o, 2, cli.Span{Text: "Effect  Permanently retire the rollback path after live-owner verification."}))
	} else {
		out.WriteString(cli.TerminalLine(o, 2, cli.Span{Text: "Proxy and every credential endpoint participant must remain stopped and drained."}))
	}
	out.WriteByte('\n')
	return out.String()
}

func terminalEndpoint(result v2EndpointResult, o cli.TerminalOptions) string {
	var out strings.Builder
	out.WriteString(cli.TerminalLine(o, 0, cli.Span{Text: "Credential endpoint", Role: cli.Heading}, cli.Span{Text: " · "}, terminalEndpointState(result.State)))
	out.WriteString(cli.TerminalLine(o, 2, cli.Span{Text: "Path    "}, cli.Span{Text: result.Path, Role: cli.Identity}))
	out.WriteString(cli.TerminalLine(o, 2, cli.Span{Text: "Kind    "}, cli.Span{Text: result.Kind, Role: cli.Identity}))
	out.WriteString(cli.TerminalLine(o, 2, cli.Span{Text: "Ticket  "}, cli.Span{Text: result.TicketID, Role: cli.Identity}))
	out.WriteByte('\n')
	if result.Kind == "snapshot" {
		out.WriteString(cli.TerminalLine(o, 0, cli.Span{Text: "Identity observed."}))
	} else {
		out.WriteString(cli.TerminalLine(o, 0, cli.Span{Text: "Ticket retained."}))
	}
	out.WriteString(cli.TerminalLine(o, 0, cli.Span{Text: "Use --json to retain the exact structured proof.", Role: cli.Secondary}))
	return out.String()
}
