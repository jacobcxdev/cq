package main

import (
	"fmt"
	"strings"

	"github.com/jacobcxdev/cq/internal/cli"
)

func terminalModelProvider(provider string) string {
	switch provider {
	case "claude":
		return "Claude"
	case "codex":
		return "Codex"
	default:
		return provider
	}
}

func terminalModelSource(source string) string {
	switch source {
	case "native":
		return "Native"
	case "overlay":
		return "Overlay"
	default:
		return source
	}
}

func terminalModelKnownAbsence(value *string) cli.Span {
	if value == nil || *value == "" {
		return cli.Span{Text: "none"}
	}
	return cli.Span{Text: *value, Role: cli.Identity}
}

func terminalModelObserved(value *string) cli.Span {
	if value == nil || *value == "" {
		return cli.Span{Text: "unknown", Role: cli.Attention}
	}
	return cli.Span{Text: *value, Role: cli.Identity}
}

func terminalModelNumber(value *int, suffix string) cli.Span {
	if value == nil {
		return cli.Span{Text: "unknown", Role: cli.Attention}
	}
	if *value < 0 {
		return cli.Span{Text: fmt.Sprintf("%d%s", *value, suffix), Role: cli.Quantity}
	}
	return cli.Span{Text: cli.TerminalCount(uint64(*value)).Text + suffix, Role: cli.Quantity}
}

func terminalModelDetails(e ModelEntry, selection *ModelCloneSelectionResult, o cli.TerminalOptions) string {
	indent := 4
	if selection != nil {
		indent = 2
	}
	aliases := cli.Span{Text: "none"}
	if len(e.Aliases) > 0 {
		aliases = cli.Span{Text: strings.Join(e.Aliases, ", "), Role: cli.Identity}
	}
	description := cli.Span{Text: "unknown", Role: cli.Attention}
	if e.Description != nil && *e.Description != "" {
		description = cli.Span{Text: *e.Description}
	}
	metadata := terminalModelKnownAbsence(e.InferredFrom)
	mode := ""
	if selection != nil && selection.SourceID != nil {
		metadata = terminalModelKnownAbsence(selection.SourceID)
		switch selection.Mode {
		case "explicit":
			mode = "Explicit"
		case "inferred":
			mode = "Inferred"
		}
	}
	if mode != "" {
		metadata.Text += " · " + mode
	}
	fields := []struct {
		label string
		value cli.Span
	}{
		{"Provider", cli.Span{Text: terminalModelProvider(e.Provider), Role: cli.Identity}},
		{"ID", cli.Span{Text: e.ID, Role: cli.Identity}},
		{"Display name", terminalModelObserved(e.DisplayName)},
		{"Source", cli.Span{Text: terminalModelSource(e.Source), Role: cli.Identity}},
		{"Aliases", aliases},
		{"Description", description},
		{"Context", terminalModelNumber(e.ContextWindow, " tokens")},
		{"Maximum context", terminalModelNumber(e.MaxContextWindow, " tokens")},
		{"Maximum output", terminalModelNumber(e.MaxOutputTokens, " tokens")},
		{"Visibility", terminalModelObserved(e.Visibility)},
		{"Priority", terminalModelNumber(e.Priority, "")},
		{"Explicit clone", terminalModelKnownAbsence(e.CloneFrom)},
		{"Metadata source", metadata},
	}
	var out strings.Builder
	for _, f := range fields {
		// The ID appears in the list headline, while an overlay receipt names it in a field.
		if selection == nil && (f.label == "ID" || f.label == "Display name") {
			continue
		}
		out.WriteString(cli.TerminalLine(o, indent, cli.Span{Text: fmt.Sprintf("%-18s", f.label)}, f.value))
	}
	return out.String()
}

func terminalModelsList(rows []ModelEntry, provider string, o cli.TerminalOptions) string {
	spans := []cli.Span{{Text: "Models", Role: cli.Heading}, {Text: " · "}, cli.TerminalCount(uint64(len(rows)))}
	if provider != "" {
		spans = append(spans, cli.Span{Text: " · "}, cli.Span{Text: terminalModelProvider(provider), Role: cli.Identity})
	}
	text := cli.TerminalLine(o, 0, spans...)
	if len(rows) == 0 {
		empty := cli.TerminalLine(o, 2, cli.Span{Text: "No cached models."})
		if provider != "" {
			empty = cli.TerminalLine(o, 2, cli.Span{Text: "No "}, cli.Span{Text: terminalModelProvider(provider), Role: cli.Identity}, cli.Span{Text: " models in the local registry."})
		}
		return text + empty + "\n" +
			cli.TerminalLine(o, 0, cli.Span{Text: "Fetch model data", Role: cli.Heading}) +
			cli.TerminalLine(o, 2, cli.Span{Text: "cq models refresh", Role: cli.Identity})
	}
	for _, row := range rows {
		name := terminalModelObserved(row.DisplayName)
		text += "\n" + cli.TerminalLine(o, 2, cli.Span{Text: row.ID, Role: cli.Identity}, cli.Span{Text: " · "}, name)
		text += terminalModelDetails(row, nil, o)
	}
	return text
}

func terminalModelStatus(status string) cli.Span {
	switch status {
	case "complete":
		return cli.Span{Text: "Complete", Role: cli.Confirmed}
	case "partial":
		return cli.Span{Text: "Partial", Role: cli.Attention}
	case "failed":
		return cli.Span{Text: "Failed", Role: cli.Failed}
	case "unknown":
		return cli.Span{Text: "Unknown", Role: cli.Attention}
	case "refreshed":
		return cli.Span{Text: "Refreshed", Role: cli.Confirmed}
	case "written":
		return cli.Span{Text: "Written", Role: cli.Confirmed}
	case "skipped":
		return cli.Span{Text: "Skipped", Role: cli.Body}
	default:
		return cli.Span{Text: status, Role: cli.Attention}
	}
}

func terminalModelTargetName(target string) string {
	switch target {
	case "codex_cache":
		return "Codex cache"
	case "claude_capabilities":
		return "Claude capabilities"
	case "claude_picker":
		return "Claude picker"
	default:
		return target
	}
}

func terminalModelReason(reason string) cli.Span {
	switch reason {
	case "published":
		return cli.Span{Text: "Published", Role: cli.Confirmed}
	case "optional_client_absent":
		return cli.Span{Text: "Optional client absent"}
	case "write_failed":
		return cli.Span{Text: "Write failed", Role: cli.Failed}
	default:
		return cli.Span{Text: reason, Role: cli.Attention}
	}
}

func terminalModelSourceError(source ModelSourceResult, o cli.TerminalOptions) string {
	if source.ErrorCode != nil && source.Message != nil {
		return cli.TerminalLine(o, 4, cli.Span{Text: *source.ErrorCode, Role: cli.Identity}, cli.Span{Text: ": " + *source.Message})
	}
	if source.Status == "failed" {
		return cli.TerminalLine(o, 4, cli.Span{Text: "Reason: "}, cli.Span{Text: "unknown", Role: cli.Attention})
	}
	return ""
}

func terminalModelsPublication(p ModelPublication, o cli.TerminalOptions) string {
	text := cli.TerminalLine(o, 0, cli.Span{Text: "Publication", Role: cli.Heading}, cli.Span{Text: " · "}, terminalModelStatus(p.Status), cli.Span{Text: " · via "}, cli.Span{Text: p.Via, Role: cli.Identity})
	if p.Status == "unknown" || (len(p.Targets) == 0 && p.Status == "failed") {
		text += cli.TerminalLine(o, 2, cli.Span{Text: "Models: "}, cli.Span{Text: "unknown", Role: cli.Attention})
	} else {
		text += cli.TerminalLine(o, 2, cli.Span{Text: "Models: "}, cli.TerminalCount(uint64(p.ActiveCount)), cli.Span{Text: " active"})
	}
	text += "\n" + cli.TerminalLine(o, 0, cli.Span{Text: "Sources", Role: cli.Heading})
	if len(p.Sources) == 0 {
		text += cli.TerminalLine(o, 2, cli.Span{Text: "No refresh source results."})
	} else if o.Width >= 58 {
		text += cli.TerminalLine(o, 2, cli.Span{Text: "Provider  Result     Native models  Malformed rows", Role: cli.Heading})
		for _, source := range p.Sources {
			native, malformed := cli.TerminalCount(uint64(source.NativeCount)), cli.TerminalCount(uint64(source.MalformedCount))
			if source.Status == "failed" {
				native, malformed = cli.Span{Text: "unknown", Role: cli.Attention}, cli.Span{Text: "unknown", Role: cli.Attention}
			}
			text += cli.TerminalLine(o, 2,
				cli.Span{Text: fmt.Sprintf("%-10s", terminalModelProvider(source.Provider)), Role: cli.Identity},
				cli.Span{Text: fmt.Sprintf("%-11s", terminalModelStatus(source.Status).Text), Role: terminalModelStatus(source.Status).Role},
				cli.Span{Text: fmt.Sprintf("%13s", native.Text), Role: native.Role}, cli.Span{Text: " "},
				cli.Span{Text: fmt.Sprintf("%15s", malformed.Text), Role: malformed.Role})
			text += terminalModelSourceError(source, o)
		}
	} else {
		for _, source := range p.Sources {
			text += cli.TerminalLine(o, 2, cli.Span{Text: terminalModelProvider(source.Provider), Role: cli.Identity}, cli.Span{Text: " · "}, terminalModelStatus(source.Status))
			native, malformed := cli.TerminalCount(uint64(source.NativeCount)), cli.TerminalCount(uint64(source.MalformedCount))
			if source.Status == "failed" {
				native, malformed = cli.Span{Text: "unknown", Role: cli.Attention}, cli.Span{Text: "unknown", Role: cli.Attention}
			}
			text += cli.TerminalLine(o, 4, cli.Span{Text: "Native models: "}, native)
			text += cli.TerminalLine(o, 4, cli.Span{Text: "Malformed rows: "}, malformed)
			text += terminalModelSourceError(source, o)
		}
	}
	text += "\n" + cli.TerminalLine(o, 0, cli.Span{Text: "Client caches", Role: cli.Heading})
	if len(p.Targets) == 0 {
		text += cli.TerminalLine(o, 2, cli.Span{Text: "No client cache publication results."})
	}
	for _, target := range p.Targets {
		text += cli.TerminalLine(o, 2, cli.Span{Text: terminalModelTargetName(target.Target), Role: cli.Identity}, cli.Span{Text: " · "}, terminalModelStatus(target.Status), cli.Span{Text: " · "}, terminalModelReason(target.Reason))
		text += cli.TerminalLine(o, 4, cli.Span{Text: target.Path, Role: cli.Identity})
		if target.ErrorCode != nil && target.Message != nil {
			text += cli.TerminalLine(o, 4, cli.Span{Text: *target.ErrorCode, Role: cli.Identity}, cli.Span{Text: ": " + *target.Message})
		}
	}
	return text
}

func terminalModelsOverlayAdd(entry ModelEntry, selection ModelCloneSelectionResult, p ModelPublication, exit int, o cli.TerminalOptions) string {
	spans := []cli.Span{{Text: "Model overlay", Role: cli.Heading}, {Text: " · "}, {Text: "Saved", Role: cli.Confirmed}}
	if exit != 0 {
		status := "Publication incomplete"
		if p.Status == "complete" {
			status = "Command incomplete"
		}
		spans = append(spans, cli.Span{Text: " · "}, cli.Span{Text: status, Role: cli.Attention})
	}
	text := cli.TerminalLine(o, 0, spans...) + terminalModelDetails(entry, &selection, o)
	return text + "\n" + terminalModelsPublication(p, o)
}

func terminalModelsOverlayRemove(provider, id string, p ModelPublication, exit int, o cli.TerminalOptions) string {
	spans := []cli.Span{{Text: "Model overlay", Role: cli.Heading}, {Text: " · "}, {Text: "Removed", Role: cli.Confirmed}}
	if exit != 0 {
		status := "Publication incomplete"
		if p.Status == "complete" {
			status = "Command incomplete"
		}
		spans = append(spans, cli.Span{Text: " · "}, cli.Span{Text: status, Role: cli.Attention})
	}
	text := cli.TerminalLine(o, 0, spans...)
	text += cli.TerminalLine(o, 2, cli.Span{Text: "Provider  "}, cli.Span{Text: terminalModelProvider(provider), Role: cli.Identity})
	text += cli.TerminalLine(o, 2, cli.Span{Text: "ID        "}, cli.Span{Text: id, Role: cli.Identity})
	return text + "\n" + terminalModelsPublication(p, o)
}

func terminalModelsOverlayPrune(removed []ModelIdentity, p ModelPublication, exit int, o cli.TerminalOptions) string {
	text := cli.TerminalLine(o, 0, cli.Span{Text: "Model overlays", Role: cli.Heading}, cli.Span{Text: " · Pruned "}, cli.TerminalCount(uint64(len(removed))))
	if exit != 0 {
		text += cli.TerminalLine(o, 2, cli.Span{Text: "Publication incomplete", Role: cli.Attention})
	}
	if len(removed) == 0 {
		text += cli.TerminalLine(o, 2, cli.Span{Text: "No redundant overlays."})
	} else {
		for _, row := range removed {
			text += cli.TerminalLine(o, 2, cli.Span{Text: terminalModelProvider(row.Provider), Role: cli.Identity}, cli.Span{Text: "  "}, cli.Span{Text: row.ID, Role: cli.Identity})
		}
	}
	return text + "\n" + terminalModelsPublication(p, o)
}
