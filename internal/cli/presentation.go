package cli

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const cliSchemaVersion = 2

var (
	semverPattern   = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-(?:0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*))*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
	revisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

type versionData struct {
	Version          string  `json:"version"`
	Revision         *string `json:"revision"`
	Dirty            *bool   `json:"dirty"`
	CLISchemaVersion int     `json:"cli_schema_version"`
}

type completionData struct {
	Shell  string `json:"shell"`
	Script string `json:"script"`
}

func VersionOutcome(info BuildInfo) Outcome {
	version := info.Version
	if !semverPattern.MatchString(version) {
		version = "dev"
	}
	buildRevision := info.Revision
	if buildRevision != nil && !revisionPattern.MatchString(*buildRevision) {
		buildRevision = nil
	}
	revision := "unknown"
	if buildRevision != nil {
		revision = *buildRevision
	}
	build := versionData{
		Version:          version,
		Revision:         buildRevision,
		Dirty:            info.Dirty,
		CLISchemaVersion: cliSchemaVersion,
	}
	data, err := json.Marshal(build)
	if err != nil {
		panic(err)
	}
	return Outcome{
		ExitCode: 0,
		Data:     data,
		Human:    fmt.Sprintf("cq %s\nRevision: %s\nCLI schema: %d\n", version, revision, cliSchemaVersion),
		Terminal: func(options TerminalOptions) string { return terminalVersion(build, options) },
	}
}

func CompletionOutcome(shell string) Outcome {
	script, ok := Completion(shell)
	if !ok {
		return Outcome{
			ExitCode: 2,
			Errors: []Diagnostic{{
				Code:    "invalid_argument",
				Message: "Invalid shell: expected one of bash, zsh, fish.",
			}},
		}
	}
	data, err := json.Marshal(completionData{Shell: shell, Script: script})
	if err != nil {
		panic(err)
	}
	return Outcome{ExitCode: 0, Data: data, Human: script}
}

func terminalVersion(data versionData, options TerminalOptions) string {
	revision := Span{Text: "unknown", Role: Attention}
	if data.Revision != nil {
		revision = Span{Text: *data.Revision, Role: Identity}
	}
	source := Span{Text: "unknown", Role: Attention}
	if data.Dirty != nil {
		source = Span{Text: "Clean", Role: Identity}
		if *data.Dirty {
			source = Span{Text: "Dirty", Role: Attention}
		}
	}
	return TerminalLine(options, 0, Span{Text: "CQ ", Role: Heading}, Span{Text: data.Version, Role: Identity}) +
		TerminalLine(options, 2, Span{Text: "Revision    "}, revision) +
		TerminalLine(options, 2, Span{Text: "Source      "}, source) +
		TerminalLine(options, 2, Span{Text: "CLI schema  "}, Span{Text: fmt.Sprint(data.CLISchemaVersion), Role: Identity})
}

// terminalDiagnostics groups structured causes without interpreting message text.
// The exit code alone cannot establish a successful or rolled-back operation.
func terminalDiagnostics(path string, outcome Outcome, options TerminalOptions) string {
	diagnostics := append([]Diagnostic{}, outcome.Errors...)
	diagnostics = append(diagnostics, outcome.Warnings...)
	// The dispatcher retains its generic interruption in the machine resource.
	// Authentication supplies a more specific terminal primary; retain all
	// other causes without printing the same interruption twice.
	if path == "auth refresh" && len(diagnostics) > 1 && diagnostics[0].Code == "interrupted" {
		for i, diagnostic := range diagnostics[1:] {
			if diagnostic.Code == "auth_interrupted" {
				index := i + 1
				diagnostics = append([]Diagnostic{diagnostic}, append(diagnostics[1:index], diagnostics[index+1:]...)...)
				break
			}
		}
	}
	if len(diagnostics) == 0 {
		return ""
	}
	primary := diagnostics[0]
	marker, role := "Error", Failed
	if len(outcome.Errors) == 0 {
		marker, role = "Warning", Attention
	}
	switch primary.Code {
	case "check_partial", "check_timeout", "auth_refresh_partial", "auth_interrupted", "account_timeout", "gemini_account_timeout", "account_activation_partial", "account_login_partial", "account_login_postcheck_partial", "account_removal_partial", "codex_reset_inventory_partial", "codex_reset_postcheck_partial", "codex_reset_recommendation_incomplete", "codex_reset_timeout", "models_refresh_partial", "trace_history_gap":
		marker, role = "Warning", Attention
	}
	command := "cq"
	if path != "" {
		command += " " + path
	}
	var out strings.Builder
	if path == "codex reset use" && outcome.Terminal != nil {
		out.WriteByte('\n')
	}
	if primary.Code == "unknown_command" {
		out.WriteString(TerminalLine(options, 0, Span{Text: marker, Role: role}, Span{Text: " · Unknown command"}))
		out.WriteString(TerminalLine(options, 2, Span{Text: primary.Message}))
		out.WriteString(TerminalLine(options, 2, Span{Text: "Code: "}, Span{Text: primary.Code, Role: Identity}, Span{Text: " · Command: "}, Span{Text: command, Role: Identity}))
	} else {
		if primary.Code == "models_clone_not_found" {
			// The model adapter owns this fixed recovery clause. Keep its
			// diagnostic and machine text intact while giving the commands a
			// separate, copyable terminal line.
			before, after, found := strings.Cut(primary.Message, "; run ")
			if found {
				out.WriteString(TerminalLine(options, 0, Span{Text: marker, Role: role}, Span{Text: " · " + before + ";"}))
				first, second, commands := strings.Cut(after, " and ")
				if commands && strings.HasPrefix(first, "cq models ") && strings.HasPrefix(second, "cq models ") {
					out.WriteString(TerminalLine(options, 2, Span{Text: "run "}, Span{Text: first, Role: Identity}, Span{Text: " and "}, Span{Text: strings.TrimSuffix(second, "."), Role: Identity}, Span{Text: "."}))
				} else {
					out.WriteString(TerminalLine(options, 2, Span{Text: "run " + after}))
				}
			} else {
				out.WriteString(TerminalLine(options, 0, Span{Text: marker, Role: role}, Span{Text: " · " + primary.Message}))
			}
		} else {
			out.WriteString(TerminalLine(options, 0, Span{Text: marker, Role: role}, Span{Text: " · " + primary.Message}))
		}
		out.WriteString(TerminalLine(options, 2, Span{Text: "Code: "}, Span{Text: primary.Code, Role: Identity}))
		out.WriteString(TerminalLine(options, 2, Span{Text: "Command: "}, Span{Text: command, Role: Identity}))
	}
	for _, diagnostic := range diagnostics[1:] {
		label := "Cause: "
		if diagnostic.Code == "deprecated_alias" || diagnostic.Code == "deprecated_option" {
			label = "Deprecation: "
		}
		out.WriteString(TerminalLine(options, 2, Span{Text: label}, Span{Text: diagnostic.Code, Role: Identity}))
		out.WriteString(TerminalLine(options, 4, Span{Text: diagnostic.Message}))
	}
	return out.String()
}
