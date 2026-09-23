package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/cli"
)

// terminalService presents only observations returned by the selected manager.
// An unavailable manager has no component receipt to display.
func terminalService(data v2ServiceData, exit int, o cli.TerminalOptions) string {
	components := make([]v2ServiceComponent, 0, len(data.Components))
	for _, component := range data.Components {
		if component.Manager != nil || component.State != "indeterminate" || component.Installed || component.Enabled != nil {
			components = append(components, component)
		}
	}
	if len(components) == 0 {
		return ""
	}
	var out strings.Builder
	out.WriteString(cli.TerminalLine(o, 0, cli.Span{Text: "CQ services", Role: cli.Heading}, cli.Span{Text: " · " + string(data.Action)}, func() cli.Span {
		if exit != 0 && data.Rollback == "failed" {
			return cli.Span{Text: " · incomplete", Role: cli.Attention}
		}
		return cli.Span{}
	}(), cli.Span{Text: "    "}, cli.Span{Text: string(data.Component), Role: cli.Identity}))
	out.WriteByte('\n')
	rows := make([][]cli.Span, 0, len(components))
	for _, c := range components {
		stateRole := cli.Body
		switch {
		case c.State == "failed":
			stateRole = cli.Failed
		case c.State == "indeterminate":
			stateRole = cli.Attention
		case exit == 0 && data.Action == serviceUninstall && c.State == "absent":
			stateRole = cli.Confirmed
		case exit == 0 && data.Action == serviceStop && c.State == "stopped":
			stateRole = cli.Confirmed
		}
		health := terminalServiceBool(c.Healthy)
		if c.Healthy != nil && *c.Healthy {
			health.Role = cli.Confirmed
		} else if c.State == "failed" {
			health.Role = cli.Failed
		}
		rows = append(rows, []cli.Span{
			{Text: string(c.ID), Role: cli.Identity},
			{Text: c.State, Role: stateRole},
			terminalServiceBool(c.Enabled),
			health,
			terminalServicePID(c),
		})
	}
	out.WriteString(cli.TerminalTable(o, []string{"Component", "State", "Auto-start", "Health", "PID"}, rows))
	for _, c := range components {
		out.WriteByte('\n')
		out.WriteString(cli.TerminalLine(o, 0, cli.Span{Text: string(c.ID), Role: cli.Heading}))
		fields := []struct {
			label string
			value cli.Span
		}{
			{"Installed", terminalServiceInstalled(c)},
			{"Manager", terminalServiceOptional(c.Manager, true)},
			{"Owner", terminalServiceOwner(c)},
			{"Executable", terminalServiceOptional(c.Executable, false)},
			{"Last run", terminalServiceLastRun(c.LastRunAt, o)},
			{"Last exit", terminalServiceLastExit(c.LastExitCode)},
			{"Error", terminalServiceError(c.ErrorCode)},
			{"Config", terminalServiceOptional(c.ConfigDir, false)},
			{"State", terminalServiceOptional(c.StateDir, false)},
			{"Cache", terminalServiceOptional(c.CacheDir, false)},
			{"Runtime", terminalServiceOptional(c.RuntimeDir, false)},
			{"Logs", terminalServiceOptional(c.LogDir, false)},
		}
		for _, field := range fields {
			out.WriteString(cli.TerminalLine(o, 2, cli.Span{Text: fmt.Sprintf("%-12s", field.label)}, field.value))
		}
	}
	out.WriteByte('\n')
	if exit == 0 {
		switch data.Action {
		case serviceRestart:
			if len(components) == 1 && components[0].ID == serviceRefresh && components[0].Enabled != nil && !*components[0].Enabled && components[0].LastExitCode != nil && *components[0].LastExitCode == 0 {
				out.WriteString(cli.TerminalLine(o, 0, cli.Span{Text: "Requested run completed successfully; automatic starts remain disabled."}))
			}
		case serviceStop:
			out.WriteString(cli.TerminalLine(o, 0, cli.Span{Text: "Selected services stopped or absent; automatic starts disabled where registered."}))
		case serviceUninstall:
			out.WriteString(cli.TerminalLine(o, 0, cli.Span{Text: "Selected registrations absent; credentials and user data retained."}))
		}
	}
	rollbackRole := cli.Secondary
	switch data.Rollback {
	case "failed":
		rollbackRole = cli.Failed
	case "restored":
		rollbackRole = cli.Attention
	}
	out.WriteString(cli.TerminalLine(o, 0, cli.Span{Text: "Rollback · "}, cli.Span{Text: data.Rollback, Role: rollbackRole}))
	return out.String()
}

func terminalServiceBool(value *bool) cli.Span {
	if value == nil {
		return cli.Span{Text: "unknown", Role: cli.Attention}
	}
	return terminalServiceYesNo(*value)
}

func terminalServiceYesNo(value bool) cli.Span {
	if value {
		return cli.Span{Text: "yes"}
	}
	return cli.Span{Text: "no"}
}

func terminalServiceInstalled(c v2ServiceComponent) cli.Span {
	if c.State == "indeterminate" && c.Enabled == nil {
		return cli.Span{Text: "unknown", Role: cli.Attention}
	}
	return terminalServiceYesNo(c.Installed)
}

func terminalServiceOwner(c v2ServiceComponent) cli.Span {
	if c.State == "indeterminate" && c.Owner == "none" {
		return cli.Span{Text: "unknown", Role: cli.Attention}
	}
	return cli.Span{Text: c.Owner, Role: cli.Identity}
}

func terminalServicePID(c v2ServiceComponent) cli.Span {
	if c.PID != nil {
		return cli.Span{Text: fmt.Sprint(*c.PID), Role: cli.Quantity}
	}
	if c.State == "indeterminate" {
		return cli.Span{Text: "unknown", Role: cli.Attention}
	}
	return cli.Span{Text: "—", Role: cli.Secondary}
}

func terminalServiceOptional(value *string, identity bool) cli.Span {
	if value == nil {
		return cli.Span{Text: "unknown", Role: cli.Attention}
	}
	role := cli.Body
	if identity {
		role = cli.Identity
	}
	return cli.Span{Text: *value, Role: role}
}

func terminalServiceLastRun(value *string, o cli.TerminalOptions) cli.Span {
	if value == nil {
		return cli.Span{Text: "unknown", Role: cli.Attention}
	}
	when, err := time.Parse(time.RFC3339Nano, *value)
	if err != nil {
		return cli.Span{Text: "unknown", Role: cli.Attention}
	}
	stamp := cli.TerminalTimestamp(when, o)
	if o.Now.IsZero() {
		return stamp
	}
	relative, _, _ := strings.Cut(cli.TerminalTime(when, o, false).Text, " · ")
	if relative == "0ms ago" {
		relative = "0s ago"
	}
	return cli.Span{Text: relative + " · " + stamp.Text, Role: cli.TimeValue}
}

func terminalServiceLastExit(value *int) cli.Span {
	if value == nil {
		return cli.Span{Text: "unknown", Role: cli.Attention}
	}
	role := cli.Body
	if *value != 0 {
		role = cli.Failed
	}
	return cli.Span{Text: fmt.Sprint(*value), Role: role}
}

func terminalServiceError(value *string) cli.Span {
	if value == nil || *value == "" {
		return cli.Span{Text: "—", Role: cli.Secondary}
	}
	return cli.Span{Text: *value, Role: cli.Failed}
}
