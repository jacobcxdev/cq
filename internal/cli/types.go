package cli

import (
	"context"
	"encoding/json"
	"io"
)

type Diagnostic struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Invocation struct {
	Path           string
	Arguments      map[string][]string
	Options        map[string][]string
	Supplied       map[string]bool
	JSON           bool
	Presentation   string
	Warnings       []Diagnostic
	LegacySelector string
}

type Outcome struct {
	ExitCode int
	Data     json.RawMessage
	Errors   []Diagnostic
	Warnings []Diagnostic
	Human    string
	Terminal TerminalRenderer
	// TerminalResult and ConfirmedReceipt affect only TTY diagnostics.
	TerminalResult   string
	ConfirmedReceipt bool
	Streamed         bool // stdout ownership consumed by terminal write or failed stream write
}

type Session struct {
	In          io.Reader
	Out         io.Writer
	Err         io.Writer
	Interactive bool
	OutTerminal TerminalOptions
	ErrTerminal TerminalOptions
	BuildInfo   BuildInfo
}

type Handler func(context.Context, Invocation, *Session) Outcome
type Lookup func(string) (Handler, bool)

type ParameterSpec struct {
	Name       string
	Type       string
	Short      string
	Metavar    string
	Choices    []string
	Default    []string
	Required   bool
	Repeatable bool
}

type CommandSpec struct {
	Path        string
	Kind        string
	Options     []ParameterSpec
	Positionals []ParameterSpec
}

type BuildInfo struct {
	Version  string
	Revision *string
	Dirty    *bool
}
