package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/dots"
)

func lookupV2Dots(path string) (cli.Handler, bool) {
	switch path {
	case "codex dots install", "codex dots start", "codex dots stop", "codex dots status", "codex dots sync", "codex dots list", "codex dots prime", "codex dots broadcast", "codex dots board":
		return handleV2Dots, true
	}
	return nil, false
}

func handleV2Dots(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome {
	return handleV2DotsWithRunner(ctx, inv, session, defaultDotsRunner())
}

// The released runner retains ownership of installation protection and native
// launch preparation. Capture its domain receipt before its legacy encoder so
// the public command emits exactly one v2 document.
func handleV2DotsWithRunner(ctx context.Context, inv cli.Invocation, _ *cli.Session, runner dotsRunner) cli.Outcome {
	options, err := v2DotsOptions(inv)
	if err != nil {
		return v2SelectionFailure(2, "dots_invalid_argument", err.Error())
	}
	if ctx.Err() != nil {
		return v2DotsFailure(ctx.Err())
	}
	operation := strings.TrimPrefix(inv.Path, "codex dots ")
	var receipt map[string]any
	run := runner.run
	runner.run = func(ctx context.Context, controller dots.Controller, request dots.Request) (map[string]any, error) {
		var err error
		receipt, err = run(ctx, controller, request)
		return nil, err
	}
	err = runner.execute(ctx, options, operation, false, io.Discard, io.Discard)
	out := cli.Outcome{}
	if err != nil {
		out = v2DotsFailure(err)
	}
	data := v2DotsData{Operation: operation}
	if receipt != nil {
		encoded, encodeErr := json.Marshal(receipt)
		if encodeErr != nil || json.Unmarshal(encoded, &data.Result) != nil {
			return v2SelectionFailure(1, "dots_result_invalid", "The Dots operation returned an invalid receipt; inspect state before retrying.")
		}
		if board, observed := receipt["board"]; observed && board == nil && err == nil {
			bound := false
			data.Result.Board = &v2DotBoard{Bound: &bound}
		}
	} else if err == nil {
		return v2SelectionFailure(1, "dots_result_missing", "The Dots operation returned no receipt; inspect state before retrying.")
	}
	if data.Result != nil && data.Result.incomplete() {
		out.Warnings = append(out.Warnings, cli.Diagnostic{Code: "dots_incomplete", Message: "Dots has pending or incomplete work; inspect the receipt before retrying."})
	}
	out.Data, err = cli.EncodeData(data)
	if err != nil {
		return v2SelectionFailure(1, "dots_result_invalid", "The Dots operation returned an invalid receipt; inspect state before retrying.")
	}
	if data.Result != nil {
		out.Human = terminalDots(data, cli.TerminalOptions{})
		out.Terminal = func(options cli.TerminalOptions) string { return terminalDots(data, options) }
	}
	return out
}

// Validate all argv-only requirements before home, filesystem or service access.
func v2DotsOptions(inv cli.Invocation) (*CodexDotsCmd, error) {
	value := func(name string) string {
		if values := inv.Options[name]; len(values) != 0 {
			return values[0]
		}
		return ""
	}
	argument := func(name string) string {
		if values := inv.Arguments[name]; len(values) != 0 {
			return values[0]
		}
		return ""
	}
	options := &CodexDotsCmd{Root: value("root")}
	operation := strings.TrimPrefix(inv.Path, "codex dots ")
	if options.Root != "" && (!filepath.IsAbs(options.Root) || filepath.Clean(options.Root) != options.Root) {
		return nil, errors.New("Dots root must be a clean absolute path.")
	}
	switch operation {
	case "install":
		options.Install = CodexDotsInstallCmd{Mode: value("mode"), App: value("app"), SystemAuth: value("system-auth"), CQAccounts: value("cq-accounts"), AccountSource: value("account-source")}
		if options.Install.Mode == "" {
			options.Install.Mode = "native"
		}
		if options.Install.App == "" {
			options.Install.App = "/Applications/ChatGPT.app"
		}
		if options.Install.Mode != "native" && options.Install.Mode != "isolated" {
			return nil, errors.New("Dots mode must be native or isolated.")
		}
		for _, path := range []string{options.Install.App, options.Install.SystemAuth, options.Install.CQAccounts, options.Install.AccountSource} {
			if path != "" && (!filepath.IsAbs(path) || filepath.Clean(path) != path) {
				return nil, errors.New("Dots install paths must be clean and absolute.")
			}
		}
	case "prime":
		options.Prime = CodexDotsPrimeCmd{Dot: argument("dot"), All: value("all") == "true"}
		if options.Prime.All == (options.Prime.Dot != "") || options.Prime.Dot != "" && strings.TrimSpace(options.Prime.Dot) == "" {
			return nil, errors.New("Choose a stable Dot handle, an unambiguous name, or --all.")
		}
	case "broadcast":
		options.Broadcast.Message = argument("message")
		if strings.TrimSpace(options.Broadcast.Message) == "" || !utf8.ValidString(options.Broadcast.Message) || utf8.RuneCountInString(options.Broadcast.Message) > 3000 {
			return nil, errors.New("Broadcast message must contain 1 to 3000 characters.")
		}
	case "board":
		options.Board = CodexDotsBoardCmd{Thread: argument("thread"), Clear: value("clear") == "true"}
		if options.Board.Clear && options.Board.Thread != "" {
			return nil, errors.New("Choose a board thread or --clear.")
		}
		if options.Board.Thread != "" && !dotsBoardThread.MatchString(strings.TrimPrefix(options.Board.Thread, "codex://threads/")) {
			return nil, errors.New("Board must be an exact local thread ID or codex://threads/ID link.")
		}
	case "start", "stop", "status", "sync", "list":
	default:
		return nil, errors.New("Unsupported Dots operation.")
	}
	return options, nil
}

func v2DotsFailure(err error) cli.Outcome {
	if out, ok := v2EnvironmentFailure(err); ok {
		return out
	}
	switch {
	case errors.Is(err, context.Canceled):
		return v2SelectionFailure(130, "interrupted", "Operation interrupted; inspect state before retrying.")
	case errors.Is(err, context.DeadlineExceeded):
		return v2SelectionFailure(7, "dots_timeout", "The Dots operation timed out; inspect state before retrying.")
	default:
		return v2SelectionFailure(1, "dots_operation_failed", "The Dots operation failed; inspect cq codex dots status before retrying.")
	}
}

type v2DotsData struct {
	Operation string        `json:"operation"`
	Result    *v2DotsResult `json:"result"`
}

// Optional fields preserve the released receipt's distinction between absent
// observations and explicit false values. No untyped provider data is emitted.
type v2DotsResult struct {
	Installed                  *bool                  `json:"installed,omitempty"`
	Started                    *bool                  `json:"started,omitempty"`
	Stopped                    *bool                  `json:"stopped,omitempty"`
	NativeCatalogueReady       *bool                  `json:"native_catalog_ready,omitempty"`
	OnboardingRequired         *bool                  `json:"onboarding_required,omitempty"`
	RestartRequired            *bool                  `json:"restart_required,omitempty"`
	SystemAppModified          *bool                  `json:"system_app_modified,omitempty"`
	AccountWatcherStarted      *bool                  `json:"account_watcher_started,omitempty"`
	AccountWatcherExisting     *bool                  `json:"account_watcher_existing,omitempty"`
	RelayStarted               *bool                  `json:"relay_started,omitempty"`
	RelayExisting              *bool                  `json:"relay_existing,omitempty"`
	SecondaryExecutorsExisting *bool                  `json:"secondary_executors_existing,omitempty"`
	CatalogueChanged           *bool                  `json:"catalogue_changed,omitempty"`
	DiscoveryComplete          *bool                  `json:"discovery_complete,omitempty"`
	DiscoveryRetryable         *bool                  `json:"discovery_retryable,omitempty"`
	Profiles                   *int                   `json:"profiles,omitempty"`
	ResearchComponents         *[]string              `json:"research_components,omitempty"`
	ControllerRuntime          string                 `json:"controller_runtime,omitempty"`
	Runtime                    string                 `json:"runtime,omitempty"`
	DockLauncher               string                 `json:"dock_launcher,omitempty"`
	Reason                     string                 `json:"reason,omitempty"`
	Accounts                   *[]dots.AccountOutcome `json:"accounts,omitempty"`
	Dots                       *[]v2Dot               `json:"dots,omitempty"`
	Priming                    *[]v2DotDelivery       `json:"priming,omitempty"`
	Awareness                  *[]v2DotDelivery       `json:"awareness,omitempty"`
	Notices                    *[]v2DotDelivery       `json:"notices,omitempty"`
	Broadcast                  *v2DotBroadcast        `json:"broadcast,omitempty"`
	Board                      *v2DotBoard            `json:"board,omitempty"`
}

type v2Dot struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Account string `json:"account"`
	Setup   string `json:"setup"`
}

type v2DotDelivery struct {
	Target      string   `json:"target"`
	DisplayName string   `json:"display_name,omitempty"`
	Outcome     string   `json:"outcome"`
	MessageID   string   `json:"message_id,omitempty"`
	FailureType string   `json:"failure_type,omitempty"`
	Error       string   `json:"error,omitempty"`
	Skipped     *bool    `json:"skipped,omitempty"`
	Manual      *bool    `json:"manual,omitempty"`
	At          *float64 `json:"at,omitempty"`
}

type v2DotBroadcast struct {
	ID         string          `json:"id"`
	Deliveries []v2DotDelivery `json:"deliveries"`
}

type v2DotBoard struct {
	ID       string `json:"id,omitempty"`
	ThreadID string `json:"thread_id,omitempty"`
	Bound    *bool  `json:"bound,omitempty"`
}

func (r v2DotsResult) incomplete() bool {
	if r.RestartRequired != nil && *r.RestartRequired || r.OnboardingRequired != nil && *r.OnboardingRequired || r.Started != nil && !*r.Started || r.DiscoveryComplete != nil && !*r.DiscoveryComplete {
		return true
	}
	groups := []*[]v2DotDelivery{r.Priming, r.Awareness, r.Notices}
	if r.Broadcast != nil {
		groups = append(groups, &r.Broadcast.Deliveries)
	}
	for _, rows := range groups {
		if rows != nil {
			for _, row := range *rows {
				if row.Outcome != "accepted" && row.Outcome != "included_in_setup" {
					return true
				}
			}
		}
	}
	return false
}

func terminalDots(data v2DotsData, options cli.TerminalOptions) string {
	var out strings.Builder
	out.WriteString(cli.TerminalLine(options, 0, cli.Span{Text: "Codex Dots", Role: cli.Heading}, cli.Span{Text: " · " + data.Operation}))
	r := data.Result
	if r == nil {
		return out.String()
	}
	field := func(label, value string, role cli.Role) {
		out.WriteString(cli.TerminalLine(options, 2, cli.Span{Text: label + ": "}, cli.Span{Text: value, Role: role}))
	}
	boolean := func(label string, value *bool, positive, negative string, good, bad cli.Role) {
		if value != nil {
			text, role := negative, bad
			if *value {
				text, role = positive, good
			}
			field(label, text, role)
		}
	}
	boolean("Installed", r.Installed, "Yes", "No", cli.Confirmed, cli.Body)
	boolean("Started", r.Started, "Yes", "No", cli.Confirmed, cli.Attention)
	boolean("Stopped", r.Stopped, "Yes", "No", cli.Confirmed, cli.Attention)
	boolean("Native catalogue", r.NativeCatalogueReady, "Ready", "Not ready", cli.Confirmed, cli.Attention)
	boolean("Onboarding required", r.OnboardingRequired, "Yes", "No", cli.Attention, cli.Body)
	boolean("Restart required", r.RestartRequired, "Yes", "No", cli.Attention, cli.Body)
	boolean("Discovery complete", r.DiscoveryComplete, "Yes", "No", cli.Confirmed, cli.Attention)
	boolean("Discovery retryable", r.DiscoveryRetryable, "Yes", "No", cli.Attention, cli.Body)
	boolean("Catalogue changed", r.CatalogueChanged, "Yes", "No", cli.Body, cli.Body)
	boolean("Account watcher", r.AccountWatcherStarted, "Running", "Not running", cli.Confirmed, cli.Attention)
	boolean("Relay", r.RelayStarted, "Running", "Not running", cli.Confirmed, cli.Attention)
	boolean("System app modified", r.SystemAppModified, "Yes", "No", cli.Attention, cli.Body)
	if r.Profiles != nil {
		field("Profiles", fmt.Sprint(*r.Profiles), cli.Quantity)
	}
	if r.Reason != "" {
		field("Detail", r.Reason, cli.Body)
	}
	if r.DockLauncher != "" {
		field("Launcher", r.DockLauncher, cli.Identity)
	}
	if r.Accounts != nil {
		for _, row := range *r.Accounts {
			out.WriteString(cli.TerminalLine(options, 2, cli.Span{Text: row.OwnerAlias, Role: cli.Identity}, cli.Span{Text: " · "}, cli.Span{Text: row.Status, Role: dotsStateRole(row.Status)}))
		}
	}
	if r.Dots != nil {
		if len(*r.Dots) == 0 {
			field("Dots", "None discovered", cli.Body)
		}
		for _, row := range *r.Dots {
			out.WriteString(cli.TerminalLine(options, 2, cli.Span{Text: row.Name, Role: cli.Identity}, cli.Span{Text: " · "}, cli.Span{Text: row.ID, Role: cli.Identity}))
			out.WriteString(cli.TerminalLine(options, 4, cli.Span{Text: "Account: "}, cli.Span{Text: row.Account, Role: cli.Identity}, cli.Span{Text: " · Setup: "}, cli.Span{Text: row.Setup, Role: dotsStateRole(row.Setup)}))
		}
	}
	deliveries := func(label string, rows *[]v2DotDelivery) {
		if rows == nil {
			return
		}
		out.WriteString(cli.TerminalLine(options, 0, cli.Span{Text: label, Role: cli.Heading}))
		if len(*rows) == 0 {
			field("Targets", "None", cli.Body)
		}
		for _, row := range *rows {
			out.WriteString(cli.TerminalLine(options, 2, cli.Span{Text: row.Target, Role: cli.Identity}, cli.Span{Text: " · "}, cli.Span{Text: row.Outcome, Role: dotsStateRole(row.Outcome)}))
			if row.DisplayName != "" {
				field("Name", row.DisplayName, cli.Identity)
			}
			if row.FailureType != "" {
				field("Failure", row.FailureType, cli.Attention)
			}
		}
	}
	deliveries("Priming", r.Priming)
	deliveries("Awareness", r.Awareness)
	deliveries("Board notices", r.Notices)
	if r.Broadcast != nil {
		field("Broadcast", r.Broadcast.ID, cli.Identity)
		deliveries("Deliveries", &r.Broadcast.Deliveries)
	}
	if data.Operation == "board" {
		if r.Board == nil {
			field("Board", "Unavailable", cli.Attention)
		} else if r.Board.Bound != nil && !*r.Board.Bound {
			field("Board", "Not bound", cli.Body)
		} else if r.Board.ThreadID != "" {
			field("Board", "codex://threads/"+r.Board.ThreadID, cli.Identity)
		}
	}
	return out.String()
}

func dotsStateRole(state string) cli.Role {
	switch state {
	case "ready", "accepted", "included_in_setup":
		return cli.Confirmed
	case "rejected", "failed":
		return cli.Failed
	case "unknown", "pending", "unavailable":
		return cli.Attention
	default:
		return cli.Body
	}
}
