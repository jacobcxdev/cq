package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func runtimeLine(o cli.TerminalOptions, indent, width int, label string, value cli.Span) string {
	return cli.TerminalLine(o, indent, cli.Span{Text: fmt.Sprintf("%-*s", width, label)}, value)
}

func runtimeIdentity(value string) cli.Span { return cli.Span{Text: value, Role: cli.Identity} }
func runtimeUnknown() cli.Span              { return cli.Span{Text: "unknown", Role: cli.Attention} }
func runtimeOptional(value *string) cli.Span {
	if value == nil {
		return cli.Span{Text: "—", Role: cli.Secondary}
	}
	return runtimeIdentity(*value)
}
func runtimeBool(value bool) cli.Span {
	if value {
		return runtimeIdentity("yes")
	}
	return runtimeIdentity("no")
}
func runtimePID(value *int) cli.Span {
	if value == nil {
		return cli.Span{Text: "—", Role: cli.Secondary}
	}
	return runtimeIdentity(strconv.Itoa(*value))
}

func terminalProxyHealth(data v2ProxyHealth, o cli.TerminalOptions) string {
	state := cli.Span{Text: "failed", Role: cli.Failed}
	if data.Healthy {
		state = cli.Span{Text: "healthy", Role: cli.Confirmed}
	}
	http := runtimeUnknown()
	if data.HTTPStatus != nil {
		http = runtimeIdentity(strconv.Itoa(*data.HTTPStatus))
	}
	return cli.TerminalLine(o, 0, cli.Span{Text: "Proxy health", Role: cli.Heading}, cli.Span{Text: " · "}, state) +
		runtimeLine(o, 2, 11, "Address", runtimeIdentity(data.Address)) +
		runtimeLine(o, 2, 11, "HTTP", http) +
		runtimeLine(o, 2, 11, "Reachable", runtimeBool(data.Reachable)) +
		runtimeLine(o, 2, 11, "Elapsed", cli.Span{Text: fmt.Sprintf("%dms", data.DurationMS), Role: cli.TimeValue}) +
		"\n" + cli.TerminalLine(o, 0, cli.Span{Text: "Health endpoint only; model traffic not verified."})
}

func terminalProxyOperation(data v2OperationStatus, o cli.TerminalOptions) string {
	state := runtimeIdentity(data.State)
	if data.State == "pending" {
		state.Role = cli.Attention
	}
	id := runtimeIdentity("idle")
	if data.OperationID != nil {
		id = runtimeOptional(data.OperationID)
	}
	available := runtimeBool(data.ResultAvailable)
	return cli.TerminalLine(o, 0, cli.Span{Text: "Proxy operation", Role: cli.Heading}, cli.Span{Text: " · "}, state) +
		runtimeLine(o, 2, 18, "Operation", id) +
		runtimeLine(o, 2, 18, "Phase", runtimeOptional(data.Phase)) +
		runtimeLine(o, 2, 18, "Result available", available) +
		runtimeLine(o, 2, 18, "Result digest", runtimeOptional(data.ResultDigest)) +
		runtimeLine(o, 2, 18, "Recovery support", cli.Span{Text: "unavailable", Role: cli.Secondary})
}

func terminalProxyRescue(data v2RescueState, o cli.TerminalOptions) string {
	mode := runtimeIdentity(data.Mode)
	switch data.Mode {
	case "drain", "rescue_draining", "rescue_exit_draining":
		mode.Role = cli.Attention
	}
	text := cli.TerminalLine(o, 0, cli.Span{Text: "Proxy rescue", Role: cli.Heading}, cli.Span{Text: " · "}, mode) +
		runtimeLine(o, 2, 19, "Generation", runtimeIdentity(strconv.FormatUint(data.Generation, 10))) +
		runtimeLine(o, 2, 19, "Active requests", cli.TerminalCount(data.ActiveRescueRequests)) +
		runtimeLine(o, 2, 19, "Draining sessions", cli.TerminalCount(uint64(len(data.DrainingSessions)))) + "\n"
	if len(data.DrainingSessions) == 0 {
		return text + cli.TerminalLine(o, 0, cli.Span{Text: "No draining sessions."})
	}
	text += cli.TerminalLine(o, 0, cli.Span{Text: "Draining sessions", Role: cli.Heading})
	for _, session := range data.DrainingSessions {
		text += cli.TerminalLine(o, 2, runtimeIdentity(session))
	}
	return text
}

func terminalProxyState(data v2ProxyState, incomplete bool, o cli.TerminalOptions) string {
	state := cli.Span{Text: "existing", Role: cli.Identity}
	if data.Created {
		state = cli.Span{Text: "created", Role: cli.Confirmed}
	}
	heading := []cli.Span{{Text: "Proxy state", Role: cli.Heading}, {Text: " · "}, state}
	if incomplete {
		heading = append(heading, cli.Span{Text: " · incomplete", Role: cli.Attention})
	}
	text := cli.TerminalLine(o, 0, heading...)
	restart := runtimeBool(data.RestartRequired)
	if data.RestartRequired {
		restart.Role = cli.Attention
	}
	return text + runtimeLine(o, 2, 18, "Directory", runtimeIdentity(data.StateDir)) +
		runtimeLine(o, 2, 18, "Restart required", restart)
}

func terminalProxyServeReady(data v2ProxyEvent, o cli.TerminalOptions) string {
	providers := "none"
	if len(data.Providers) > 0 {
		names := make([]string, len(data.Providers))
		for i, provider := range data.Providers {
			switch provider {
			case "claude":
				names[i] = "Claude"
			case "codex":
				names[i] = "Codex"
			default:
				names[i] = provider
			}
		}
		providers = strings.Join(names, ", ")
	}
	return cli.TerminalLine(o, 0, cli.Span{Text: "Proxy", Role: cli.Heading}, cli.Span{Text: " · listening", Role: cli.Identity}) +
		runtimeLine(o, 2, 11, "Address", runtimeIdentity(data.ListenAddress)) +
		runtimeLine(o, 2, 11, "PID", runtimeIdentity(strconv.Itoa(data.PID))) +
		runtimeLine(o, 2, 11, "Providers", runtimeIdentity(providers)) +
		"\n" + cli.TerminalLine(o, 0, cli.Span{Text: "Ctrl-C stops this foreground process."})
}

func terminalProxyServeStopped(data v2ProxyEvent, o cli.TerminalOptions) string {
	reason := runtimeOptional(data.Reason)
	return "\n" + cli.TerminalLine(o, 0, cli.Span{Text: "Proxy", Role: cli.Heading}, cli.Span{Text: " · stopped", Role: cli.Identity}) +
		runtimeLine(o, 2, 11, "Reason", reason)
}

type v2ProxyInspectorValue struct {
	Executable *string `json:"executable"`
	Version    *string `json:"version"`
	Commit     *string `json:"commit"`
	Digest     *string `json:"digest"`
}
type v2ProxyDesiredValue struct {
	Manager    *string `json:"manager"`
	Configured bool    `json:"configured"`
	Listener   *string `json:"listener"`
}
type v2ProxyServiceValue struct {
	Manager    *string `json:"manager"`
	State      *string `json:"state"`
	Executable *string `json:"executable"`
	PID        *int    `json:"pid"`
}
type v2ProxyListenerValue struct {
	State      *string `json:"state"`
	Listener   *string `json:"listener"`
	Executable *string `json:"executable"`
	PID        *int    `json:"pid"`
}
type v2ProxyProcessValue struct {
	PID        int    `json:"pid"`
	Executable string `json:"executable"`
}
type v2ProxyRuntimeValue struct {
	Reachable  bool    `json:"reachable"`
	PID        *int    `json:"pid"`
	Executable *string `json:"executable"`
	Health     *string `json:"health"`
}
type v2ProxyDataPlaneValue struct {
	Proven bool    `json:"proven"`
	Code   *string `json:"code"`
}

type runtimeField struct {
	label string
	value cli.Span
}

func runtimeFactFields(fact v2ProxyFact) []runtimeField {
	field := func(label string, value cli.Span) runtimeField { return runtimeField{label, value} }
	switch value := fact.Value.(type) {
	case v2ProxyInspectorValue:
		return []runtimeField{field("Executable", runtimeOptional(value.Executable)), field("Version", runtimeOptional(value.Version)), field("Commit", runtimeOptional(value.Commit)), field("Digest", runtimeOptional(value.Digest))}
	case v2ProxyDesiredValue:
		return []runtimeField{field("Manager", runtimeOptional(value.Manager)), field("Configured", runtimeBool(value.Configured)), field("Listener", runtimeOptional(value.Listener))}
	case v2ProxyServiceValue:
		return []runtimeField{field("Manager", runtimeOptional(value.Manager)), field("State", runtimeOptional(value.State)), field("Executable", runtimeOptional(value.Executable)), field("PID", runtimePID(value.PID))}
	case v2ProxyListenerValue:
		return []runtimeField{field("State", runtimeOptional(value.State)), field("Listener", runtimeOptional(value.Listener)), field("Executable", runtimeOptional(value.Executable)), field("PID", runtimePID(value.PID))}
	case v2ProxyProcessValue:
		return []runtimeField{field("PID", runtimeIdentity(strconv.Itoa(value.PID))), field("Executable", runtimeIdentity(value.Executable))}
	case v2ProxyRuntimeValue:
		health := runtimeOptional(value.Health)
		if value.Health != nil && *value.Health == "healthy" {
			health.Role = cli.Confirmed
		}
		return []runtimeField{field("Reachable", runtimeBool(value.Reachable)), field("PID", runtimePID(value.PID)), field("Executable", runtimeOptional(value.Executable)), field("Health", health)}
	case v2ProxyDataPlaneValue:
		return []runtimeField{field("Proven", runtimeBool(value.Proven)), field("Code", runtimeOptional(value.Code))}
	}
	return nil
}

func terminalProxyStatus(data v2ProxyStatus, o cli.TerminalOptions) string {
	state := runtimeIdentity(data.State)
	switch data.State {
	case "ready":
		state.Role = cli.Confirmed
	case "degraded":
		state.Role = cli.Failed
	case "indeterminate":
		state.Role = cli.Attention
	}
	observed := runtimeUnknown()
	if timestamp, err := time.Parse(time.RFC3339Nano, data.CollectedAt); err == nil {
		relative := strings.SplitN(cli.TerminalTime(timestamp, o, false).Text, " · ", 2)[0]
		if relative == "0ms ago" {
			relative = "0s ago"
		}
		observed = cli.Span{Text: relative + " · " + cli.TerminalTimestamp(timestamp, o).Text, Role: cli.TimeValue}
	}
	text := cli.TerminalLine(o, 0, cli.Span{Text: "Proxy", Role: cli.Heading}, cli.Span{Text: " · "}, state, cli.Span{Text: "    "}, runtimeIdentity(data.Scope)) +
		runtimeLine(o, 2, 10, "Observed", observed) +
		runtimeLine(o, 2, 10, "Elapsed", cli.Span{Text: fmt.Sprintf("%dms", data.DurationMS), Role: cli.TimeValue}) +
		runtimeLine(o, 2, 10, "State", runtimeOptional(data.StateDir)) + "\n" +
		cli.TerminalLine(o, 0, cli.Span{Text: "Evidence", Role: cli.Heading})
	for _, fact := range data.Facts {
		name := strings.ReplaceAll(fact.Name, "_", " ")
		if name != "data plane" {
			name = strings.ToUpper(name[:1]) + name[1:]
		} else {
			name = "Data plane"
		}
		state := runtimeIdentity(string(fact.State))
		if fact.State == proxy.FactUnavailable || fact.State == proxy.FactPermissionDenied || fact.State == "" {
			state.Role = cli.Attention
		}
		if fact.State == proxy.FactInvalid {
			state.Role = cli.Failed
		}
		text += cli.TerminalLine(o, 2, cli.Span{Text: name, Role: cli.Heading}, cli.Span{Text: " · "}, state)
		if fact.State != proxy.FactKnown {
			if fact.ErrorCode != nil {
				text += runtimeLine(o, 4, 12, "Code", runtimeIdentity(*fact.ErrorCode))
			}
			continue
		}
		for _, field := range runtimeFactFields(fact) {
			text += runtimeLine(o, 4, 12, field.label, field.value)
		}
	}
	return text + "\n" + cli.TerminalLine(o, 0, cli.Span{Text: "Readiness inspection does not establish model-traffic acceptance."})
}
