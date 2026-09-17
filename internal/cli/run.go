package cli

import (
	"context"
	"fmt"
)

var interruptedDiagnostic = Diagnostic{"interrupted", "Operation interrupted; inspect state before retrying."}

// Run performs syntax and pure presentation before consulting the injected
// handler registry. It never exits the process or retries an operation.
func Run(ctx context.Context, argv []string, session *Session, lookup Lookup) int {
	inv, parseErr := Parse(argv)
	if parseErr != nil {
		inv.Path = parseErr.Path
		return render(session, inv, Outcome{ExitCode: parseErr.ExitCode, Errors: []Diagnostic{parseErr.Diagnostic}})
	}
	if inv.Presentation == "help" {
		text, ok := Help(inv.Path)
		if !ok {
			return render(session, inv, internalFailure())
		}
		inv.JSON = false // help is plain text even with an explicit --json
		return render(session, inv, Outcome{Human: text})
	}
	if inv.Presentation == "version" || inv.Path == "version" {
		inv.Path = "version"
		return render(session, inv, VersionOutcome(session.BuildInfo))
	}
	if inv.Path == "completion" {
		return render(session, inv, CompletionOutcome(inv.Arguments["shell"][0]))
	}
	if ctx.Err() == context.Canceled {
		return render(session, inv, Outcome{ExitCode: 130, Errors: []Diagnostic{interruptedDiagnostic}})
	}
	if lookup == nil {
		return render(session, inv, internalFailure())
	}
	handler, ok := lookup(inv.Path)
	if !ok || handler == nil {
		return render(session, inv, internalFailure())
	}
	outcome := handler(ctx, inv, session)
	// Stream handlers own their terminal record or failed output attempt,
	// including interruption and output failures. A known receipt in Data does
	// not make a normal invocation immune to interruption during cleanup.
	if !outcome.Streamed && (ctx.Err() == context.Canceled || outcome.ExitCode == 130) {
		outcome.ExitCode = 130
		found := false
		for _, diagnostic := range outcome.Errors {
			found = found || diagnostic.Code == "interrupted"
		}
		if !found {
			outcome.Errors = append([]Diagnostic{interruptedDiagnostic}, outcome.Errors...)
		}
	}
	return render(session, inv, outcome)
}

func internalFailure() Outcome {
	return Outcome{ExitCode: 1, Errors: []Diagnostic{{"internal_error", "Operation failed because of an internal error."}}}
}

func render(session *Session, inv Invocation, outcome Outcome) int {
	warnings := append([]Diagnostic{}, inv.Warnings...)
	outcome.Warnings = append(warnings, outcome.Warnings...)
	for _, warning := range outcome.Warnings {
		if err := writeBytes(session.Err, []byte(fmt.Sprintf("cq: warning: %s\n", HumanValue(warning.Message)))); err != nil {
			return 1
		}
	}
	var err error
	switch {
	case outcome.Streamed:
		// Only stdout's terminal record is already owned by the handler.
		// Human diagnostics below still belong on stderr.
	case inv.JSON:
		err = WriteJSON(session.Out, inv.Path, outcome)
	case inv.Path == "codex proxy hook stop" && len(outcome.Data) > 0:
		// This branch encodes the actual resource as JSON, never a human
		// template containing hand-escaped protocol text.
		err = writeJSONValue(session.Out, outcome.Data)
	default:
		if outcome.Human != "" {
			err = writeBytes(session.Out, []byte(outcome.Human))
		}
	}
	if err != nil {
		// The document may already be partially written. Report only a safe
		// stderr diagnostic and never attempt a second stdout document.
		_ = writeBytes(session.Err, []byte("cq: Output could not be written.\n"))
		return 1
	}
	if !inv.JSON {
		for _, diagnostic := range outcome.Errors {
			if err := writeBytes(session.Err, []byte(fmt.Sprintf("cq: %s\n", HumanValue(diagnostic.Message)))); err != nil {
				return 1
			}
		}
	}
	return outcome.ExitCode
}
