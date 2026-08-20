package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/alecthomas/kong"
	"github.com/jacobcxdev/cq/internal/proxy"
)

var manualHelpByPath = map[string]string{
	"codex validate": "Usage: cq codex validate capture ... | http --client-build BUILD [--state-dir DIR] | websocket --client-build BUILD [--client-executable PATH] [--state-dir DIR]\n",
	"codex canary":   "Usage: cq codex canary start|status|stop\n",
	"refresh": `Usage: cq refresh

Refresh stored OAuth tokens for Claude and Codex before they expire.

Use this before long sessions or from the background agent. Expired Claude
accounts may require interactive reauth from a terminal.
`,
	"agent": `Usage: cq agent <command>

Manage the background quota refresh LaunchAgent.

Commands:
  agent install       Install background refresh agent
  agent uninstall     Uninstall background refresh agent
`,
	"agent install": `Usage: cq agent install

Install the background quota refresh LaunchAgent.

The agent periodically runs token refresh work so provider checks and proxy
routing are less likely to start from expired credentials.
`,
	"agent uninstall": `Usage: cq agent uninstall

Uninstall the background quota refresh LaunchAgent.
`,
	"proxy": `Usage: cq proxy <command>

Run and configure the local Claude and Codex API proxy.

Commands:
  proxy start         Start local Claude and Codex proxy
  proxy status        Show proxy health
  proxy install       Install proxy launch agent
  proxy uninstall     Uninstall proxy launch agent
  proxy restart       Restart proxy launch agent
  proxy validate-http Request one-shot installed HTTP validation
  proxy pin           Pin Claude proxy routing
  proxy codex-default Configure Codex routing default
  proxy prime         Manage Codex quota-window priming
  proxy endpoint      Explicitly inspect or transition the credential endpoint
`,
	"proxy start": `Usage: cq proxy start [--port PORT] [--migrate-legacy-managed]

Start local Claude and Codex proxy.

Options:
  --port PORT                    Override configured listen port for this run
  --migrate-legacy-managed       Explicitly add routing identity metadata to legacy CQ-managed records
`,
	"proxy status": `Usage: cq proxy status [--port PORT]

Show proxy health as JSON.

Options:
  --port PORT         Override configured health-check port
`,
	"proxy install": `Usage: cq proxy install

Install the proxy launch agent for the current user.
`,
	"proxy uninstall": `Usage: cq proxy uninstall

Uninstall the proxy launch agent for the current user.
`,
	"proxy restart": `Usage: cq proxy restart

Restart the proxy launch agent for the current user.
`,
	"proxy validate-http": `Usage: cq proxy validate-http --port PORT

Request one-shot installed HTTP validation from a candidate proxy service.
PORT must match the candidate service's explicit --port and cannot be live port 19280.
Shared proxy configuration is never changed by this command.
The command writes only an expiring private request and restarts that service.
The serving process owns validation and does not write readiness evidence until
every installed acceptance gate and process attestation succeeds.
`,
	"proxy pin": `Usage: cq proxy pin [--clear | <email-or-account-uuid>]

Pin Claude proxy routing to a specific account, show the current pin, or clear it.

Examples:
  cq proxy pin
  cq proxy pin user@example.com
  cq proxy pin 550e8400-e29b-41d4-a716-446655440000
  cq proxy pin --clear
`,
	"proxy codex-default": `Usage: cq proxy codex-default [--clear | <account-reference>]

Show, set, or clear CQ-owned Codex routing default.
An account reference may be a unique email, CQ alias, or opaque AccountKey.
CQ resolves it once and stores only opaque AccountKey.

The stored opaque account key is independent of Codex Desktop/system identity.
This command changes only CQ proxy configuration and never mutates Codex Bar or system authentication.
The running proxy keeps its startup value. Restart proxy to apply change.

Options:
  --clear            Clear the Codex routing default
`,
	"proxy prime": `Usage: cq proxy prime <command>

Manage automatic Codex backend quota-window priming.

Commands:
  prime status        Show current priming configuration
  prime enable        Enable priming after proxy restart
  prime disable       Disable priming after proxy restart
`,
	"proxy prime status": `Usage: cq proxy prime status

Show current Codex window priming configuration.
`,
	"proxy prime enable": `Usage: cq proxy prime enable

Enable automatic Codex window priming. Restart proxy to apply.
`,
	"proxy prime disable": `Usage: cq proxy prime disable

Disable automatic Codex window priming. Restart proxy to apply.
`,
	"proxy endpoint": `Usage: cq proxy endpoint <command>

Explicitly inspect or transition the fixed default Codex credential endpoint.
Ordinary cq and proxy startup never invoke legacy endpoint maintenance.

Commands:
  endpoint inspect-legacy      Read a refused legacy socket or pending transition
  endpoint transition-legacy   Run an explicit stopped-and-drained transition
`,
	"proxy endpoint inspect-legacy": `Usage: cq proxy endpoint inspect-legacy

Read the fixed default Codex credential endpoint without creating or changing
any endpoint, compatibility, authentication, or account state. Output is JSON.
`,
	"proxy endpoint transition-legacy": `Usage: cq proxy endpoint transition-legacy <prepare|resume|activate|finalise|rollback> [options]

Explicitly transition the fixed default legacy credential endpoint. Prepare,
resume, activate, and rollback require the proxy to remain stopped and drained.
Finalise requires the exact live candidate to pass its in-owner runtime verifier.
The deprecated commit action is unavailable and never removes rollback state.

Options:
  --snapshot-file FILE              Strict 0600 snapshot input for prepare
  --ticket-file FILE                Strict 0600 ticket input for other actions
  --confirm-stopped-and-drained      Required for prepare/resume/activate/rollback
  --confirm-candidate-healthy        Required for finalise
  --non-interactive                  Skip the TTY phrase prompt
`,
	"models": `Usage: cq models <command>

Manage the local model registry used by the proxy, Claude Code model caches,
and Codex model cache integration.

Commands:
  models list         List active registry models
  models refresh      Refresh registry data and publish caches
  models overlay      Manage user model overlays
`,
	"models list": `Usage: cq models list [--json] [--provider PROVIDER]

List active registry models.

Options:
  --json              Output JSON
  --provider PROVIDER Filter by provider: anthropic or codex
`,
	"models refresh": `Usage: cq models refresh

Refresh registry data and publish provider-specific caches.

This updates Codex model cache, Claude Code model capabilities, and Claude Code
picker options where those files are available.
`,
	"models overlay": `Usage: cq models overlay <command>

Manage user model overlays.

Overlays let cq expose model IDs before providers publish them natively.

Commands:
  overlay add         Add or update user model overlay
  overlay remove      Remove user model overlay
  overlay prune       Remove overlays now supplied natively
`,
	"models overlay add": `Usage: cq models overlay add --provider PROVIDER --id MODEL [--clone-from MODEL]

Add or update user model overlay.

Options:
  --provider PROVIDER Provider to publish under: anthropic or codex
  --id MODEL          Model ID to add
  --clone-from MODEL  Native model ID to copy metadata from
`,
	"models overlay remove": `Usage: cq models overlay remove --provider PROVIDER --id MODEL

Remove user model overlay.

Options:
  --provider PROVIDER Provider to remove from: anthropic or codex
  --id MODEL          Model ID to remove
`,
	"models overlay prune": `Usage: cq models overlay prune

Remove overlays now supplied natively by refreshed provider registries.
`,
}

func manualHelp(path []string) (string, bool) {
	help, ok := manualHelpByPath[strings.Join(path, " ")]
	return help, ok
}

func writeManualHelp(w io.Writer, path []string) error {
	help, ok := manualHelp(path)
	if !ok {
		return fmt.Errorf("no help for command path: %s", strings.Join(path, " "))
	}
	_, err := fmt.Fprint(w, help)
	return err
}

func helpRequested(args []string) bool {
	for _, arg := range args {
		if isHelpToken(arg) {
			return true
		}
	}
	return false
}

func isHelpToken(arg string) bool {
	return arg == "--help" || arg == "-h" || arg == "help"
}

// runPureGlobalInspection handles global read-only commands before any
// compatibility or configuration initialisation can create user state.
func runPureGlobalInspection(args []string, stdout, stderr io.Writer) (bool, int, error) {
	return runPureGlobalInspectionWithTarget(args, stdout, stderr, defaultProxyInspectionTarget())
}

func runPureGlobalInspectionWithTarget(args []string, stdout, stderr io.Writer, target ProxyInspectionTarget) (bool, int, error) {
	authority, err := ClassifyProxyCommand(args)
	if err != nil {
		return true, 64, err
	}
	if authority.Catalogue == "proxy" && authority.Row == "proxy_status" && !authority.Terminating {
		arguments, ok := authority.Arguments.(ProxyStatusArgumentsV1)
		if !ok {
			return true, 64, errors.New("proxy status usage")
		}
		mode := ProxyRenderHuman
		if arguments.JSON {
			mode = ProxyRenderJSON
		}
		ctx, cancel := context.WithTimeout(context.Background(), authority.Deadline.Total)
		defer cancel()
		snapshot := InspectProxy(ctx, target)
		if err := RenderProxySnapshot(stdout, snapshot, mode); err != nil {
			return true, 1, err
		}
		return true, snapshot.ExitCode, nil
	}
	if authority.Catalogue == "proxy" && authority.Row == "proxy_status_frozen" && !authority.Terminating {
		opts, parseErr := parseProxyCommandOptionsFor("proxy status", args[2:])
		if parseErr != nil {
			return true, 1, parseErr
		}
		if statusErr := runProxyStatus(opts); statusErr != nil {
			return true, 1, statusErr
		}
		return true, 0, nil
	}
	if len(args) >= 3 && args[0] == "proxy" && args[1] == "status" && args[2] == "--port" && authority.Row == "ordinary_usage_error" && !proxyStatusHasReconciledSelector(args[3:]) {
		_, parseErr := parseProxyCommandOptionsFor("proxy status", args[2:])
		if parseErr == nil {
			parseErr = errors.New("proxy status usage")
		}
		return true, 1, parseErr
	}
	if len(args) >= 2 && args[0] == "proxy" && args[1] == "status" && authority.Row == "ordinary_usage_error" && (len(args) < 3 || args[2] != "--migrate-legacy-managed") {
		return true, 64, errors.New("proxy status usage")
	}
	if result := classifyInterceptedInspection(args); result.handled {
		if len(result.helpPath) > 0 {
			writer := stdout
			if result.helpOnStderr {
				writer = stderr
			}
			if err := writeManualHelp(writer, result.helpPath); err != nil {
				return true, 1, err
			}
		}
		if result.stderrText != "" {
			if _, err := fmt.Fprint(stderr, result.stderrText); err != nil {
				return true, 1, err
			}
		}
		return true, result.exitCode, result.err
	}
	if isInterceptedCommand(args) {
		return false, 0, nil
	}
	var cli CLI
	exitCode := -1
	stdoutRecorder := &errorRecordingWriter{writer: stdout}
	stderrRecorder := &errorRecordingWriter{writer: stderr}
	options := append(cliKongOptions(),
		kong.Writers(stdoutRecorder, stderrRecorder),
		kong.Exit(func(code int) { panic(pureInspectionExit(code)) }),
	)
	parser, err := kong.New(&cli, options...)
	if err != nil {
		return true, 1, err
	}
	err = parsePureInspection(parser, args, &exitCode)
	if writeErr := errors.Join(stdoutRecorder.err, stderrRecorder.err); writeErr != nil {
		return true, 1, writeErr
	}
	if err != nil {
		var parseError *kong.ParseError
		if !errors.As(err, &parseError) {
			return true, 1, err
		}
		fatalPureInspection(parser, err, &exitCode)
		if writeErr := errors.Join(stdoutRecorder.err, stderrRecorder.err); writeErr != nil {
			return true, 1, writeErr
		}
		if exitCode < 0 {
			exitCode = 1
		}
		return true, exitCode, err
	}
	if exitCode >= 0 {
		return true, exitCode, nil
	}
	return false, 0, nil
}

func proxyStatusHasReconciledSelector(args []string) bool {
	for _, arg := range args {
		switch arg {
		case "--json", "--human", "--strict", "--timeout", "--instance-state-root":
			return true
		}
	}
	return false
}

type interceptedInspectionResult struct {
	handled      bool
	exitCode     int
	helpPath     []string
	helpOnStderr bool
	stderrText   string
	err          error
}

func classifyInterceptedInspection(args []string) interceptedInspectionResult {
	if path, message, ok := interceptedZeroArgumentUsage(args); ok {
		return interceptedInspectionResult{handled: true, exitCode: 1, helpPath: path, helpOnStderr: true, err: errors.New(message)}
	}
	if path, ok := manualHelpInspectionPath(args); ok {
		return interceptedInspectionResult{handled: true, helpPath: path}
	}
	if err := manualUsageInspectionError(args); err != nil {
		return interceptedInspectionResult{handled: true, exitCode: 1, err: err}
	}
	if err := validateInterceptedLexicalGrammar(args); err != nil {
		result := interceptedInspectionResult{handled: true, exitCode: 1, err: err}
		if len(args) >= 2 && args[0] == "proxy" && args[1] == "pin" {
			result.stderrText = "Usage: cq proxy pin [--clear | <email-or-account-uuid>]\n"
		}
		return result
	}
	return interceptedInspectionResult{}
}

func validateInterceptedLexicalGrammar(args []string) error {
	if len(args) == 0 {
		return nil
	}
	switch args[0] {
	case "refresh":
		if len(args) > 1 {
			return fmt.Errorf("refresh: unexpected arguments")
		}
	case "agent":
		if len(args) > 1 && args[1] != "install" && args[1] != "uninstall" {
			return fmt.Errorf("unknown agent command: %s", args[1])
		}
	case "proxy":
		return validateProxyLexicalGrammar(args[1:])
	case "models":
		return validateModelsLexicalGrammar(args[1:])
	case "codex":
		if len(args) > 1 && args[1] == "validate" {
			return validateCodexValidationLexicalGrammar(args[2:])
		}
		if len(args) > 1 && args[1] == "canary" {
			return validateCodexCanaryLexicalGrammar(args[2:])
		}
	}
	return nil
}

func validateProxyLexicalGrammar(args []string) error {
	if len(args) == 0 {
		return nil
	}
	switch args[0] {
	case "start":
		_, err := parseProxyCommandOptions(args[1:])
		return err
	case "status":
		_, err := parseProxyCommandOptionsFor("proxy status", args[1:])
		return err
	case "validate-http":
		options, err := parseProxyCommandOptionsFor("proxy validate-http", args[1:])
		if err != nil {
			return err
		}
		if options.Port == 0 {
			return errors.New("proxy validate-http: --port is required")
		}
		if options.Port == proxy.DefaultPort {
			return errors.New("proxy validate-http: live proxy port is forbidden")
		}
	case "pin":
		return validateProxyPinLexicalGrammar(args[1:])
	case "codex-default":
		if len(args) > 2 || (len(args) == 2 && args[1] != "--clear" && strings.HasPrefix(args[1], "-")) {
			return errors.New(proxyCodexDefaultUsageMessage)
		}
	case "prime":
		if len(args) != 2 {
			return fmt.Errorf("usage: cq proxy prime <status|enable|disable>")
		}
		if args[1] != "status" && args[1] != "enable" && args[1] != "disable" {
			return fmt.Errorf("unknown proxy prime command: %s", args[1])
		}
	case "endpoint":
		return validateProxyEndpointLexicalGrammar(args[1:])
	case "install", "uninstall", "restart":
		return nil
	default:
		return fmt.Errorf("unknown proxy command: %s", args[0])
	}
	return nil
}

func validateProxyPinLexicalGrammar(args []string) error {
	if len(args) > 1 {
		return fmt.Errorf("unexpected arguments")
	}
	if len(args) == 0 || args[0] == "--clear" {
		return nil
	}
	lower := strings.ToLower(args[0])
	if lower == "clear" || lower == "remove" {
		return fmt.Errorf("reserved word %q is not valid; did you mean --clear?", args[0])
	}
	if strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("unknown flag %q", args[0])
	}
	return nil
}

func validateProxyEndpointLexicalGrammar(args []string) error {
	if len(args) == 0 {
		return nil
	}
	switch args[0] {
	case "inspect-legacy":
		if len(args) != 1 {
			return fmt.Errorf("usage: cq proxy endpoint inspect-legacy")
		}
	case "transition-legacy":
		_, err := parseLegacyEndpointTransitionOptions(args[1:])
		return err
	default:
		return fmt.Errorf("unknown proxy endpoint command: %s", args[0])
	}
	return nil
}

func validateModelsLexicalGrammar(args []string) error {
	if len(args) == 0 {
		return nil
	}
	switch args[0] {
	case "list":
		for index := 1; index < len(args); index++ {
			switch args[index] {
			case "--json":
			case "--provider":
				if index+1 >= len(args) {
					return fmt.Errorf("models list: --provider requires a value")
				}
				if _, err := parseModelsProvider(args[index+1]); err != nil {
					return err
				}
				index++
			default:
				return fmt.Errorf("models list: unknown argument %s", args[index])
			}
		}
	case "refresh":
		return nil
	case "overlay":
		if len(args) == 1 {
			return nil
		}
		switch args[1] {
		case "add":
			_, _, _, err := parseOverlayModelFlags(args[2:], true)
			return err
		case "remove":
			_, _, _, err := parseOverlayModelFlags(args[2:], false)
			return err
		case "prune":
			return nil
		default:
			return fmt.Errorf("unknown models overlay command: %s", args[1])
		}
	default:
		return fmt.Errorf("unknown models command: %s", args[0])
	}
	return nil
}

func validateCodexValidationLexicalGrammar(args []string) error {
	if len(args) == 0 {
		return nil
	}
	switch args[0] {
	case "capture":
		return validateCodexFlagPairs(args[1:], map[string]string{
			"--input": "input", "--output": "output", "--content-encoding": "content-encoding", "--metadata": "metadata",
		}, "Codex capture", "unknown Codex capture argument", []string{"input", "output"}, false)
	case "http":
		return validateCodexFlagPairs(args[1:], map[string]string{"--client-build": "client-build", "--state-dir": "state-dir"}, "Codex HTTP validation", "unknown Codex HTTP validation argument", []string{"client-build"}, true)
	case "websocket":
		return validateCodexFlagPairs(args[1:], map[string]string{"--client-build": "client-build", "--client-executable": "client-executable", "--state-dir": "state-dir"}, "Codex WebSocket validation", "unknown Codex WebSocket validation argument", []string{"client-build"}, true)
	default:
		return fmt.Errorf("unknown Codex validation command: %s", args[0])
	}
}

func validateCodexFlagPairs(args []string, flags map[string]string, command, unknownPrefix string, required []string, trimRequired bool) error {
	seen := make(map[string]bool, len(flags))
	for index := 0; index < len(args); index += 2 {
		if index+1 >= len(args) {
			return fmt.Errorf("%s requires a value", args[index])
		}
		name, ok := flags[args[index]]
		if !ok {
			return fmt.Errorf("%s: %s", unknownPrefix, args[index])
		}
		value := args[index+1]
		if trimRequired {
			value = strings.TrimSpace(value)
		}
		seen[name] = value != ""
	}
	for _, name := range required {
		if !seen[name] {
			if command == "Codex capture" {
				return fmt.Errorf("Codex capture requires --input and --output")
			}
			return fmt.Errorf("%s requires --%s", command, name)
		}
	}
	return nil
}

func validateCodexCanaryLexicalGrammar(args []string) error {
	if len(args) == 0 {
		return nil
	}
	if len(args) != 1 {
		return fmt.Errorf("Codex canary %s takes no arguments", args[0])
	}
	if args[0] != "start" && args[0] != "status" && args[0] != "stop" {
		return fmt.Errorf("unknown Codex canary command: %s", args[0])
	}
	return nil
}

func interceptedZeroArgumentUsage(args []string) ([]string, string, bool) {
	switch {
	case len(args) == 1 && args[0] == "agent":
		return []string{"agent"}, "missing subcommand", true
	case len(args) == 1 && args[0] == "proxy":
		return []string{"proxy"}, "missing subcommand", true
	case len(args) == 1 && args[0] == "models":
		return []string{"models"}, "models: missing subcommand", true
	case len(args) == 2 && args[0] == "models" && args[1] == "overlay":
		return []string{"models", "overlay"}, "models overlay: missing subcommand", true
	case len(args) == 2 && args[0] == "proxy" && args[1] == "prime":
		return nil, "usage: cq proxy prime <status|enable|disable>", true
	default:
		return nil, "", false
	}
}

func manualUsageInspectionError(args []string) error {
	if len(args) >= 2 && args[0] == "proxy" && args[1] == "prime" {
		primeArgs := args[2:]
		if len(primeArgs) > 0 && isHelpToken(primeArgs[0]) {
			path := []string{"proxy", "prime"}
			if primeArgs[0] == "help" {
				path = append(path, primeArgs[1:]...)
			}
			if _, ok := manualHelp(path); !ok {
				return fmt.Errorf("no help for command path: %s", strings.Join(path, " "))
			}
		}
		if len(primeArgs) == 2 && helpRequested(primeArgs[1:]) {
			path := []string{"proxy", "prime", primeArgs[0]}
			if _, ok := manualHelp(path); !ok {
				return fmt.Errorf("no help for command path: %s", strings.Join(path, " "))
			}
		}
		if len(primeArgs) != 1 && helpRequested(primeArgs) {
			return fmt.Errorf("usage: cq proxy prime <status|enable|disable>")
		}
	}
	if len(args) >= 3 && args[0] == "models" && args[1] == "overlay" {
		overlayArgs := args[2:]
		if len(overlayArgs) > 0 && overlayArgs[0] == "help" {
			path := append([]string{"models", "overlay"}, overlayArgs[1:]...)
			if _, ok := manualHelp(path); !ok {
				return fmt.Errorf("no help for command path: %s", strings.Join(path, " "))
			}
		}
		if len(overlayArgs) > 1 && helpRequested(overlayArgs[1:]) && overlayArgs[0] != "add" && overlayArgs[0] != "remove" && overlayArgs[0] != "prune" {
			return fmt.Errorf("unknown models overlay command: %s", overlayArgs[0])
		}
	}
	if len(args) >= 2 && (args[0] == "agent" || args[0] == "models" || args[0] == "proxy") && args[1] == "help" {
		path := append([]string{args[0]}, args[2:]...)
		if _, ok := manualHelp(path); !ok {
			return fmt.Errorf("no help for command path: %s", strings.Join(path, " "))
		}
	}
	if len(args) >= 2 && helpRequested(args[1:]) {
		switch args[0] {
		case "agent":
			if args[1] != "install" && args[1] != "uninstall" {
				return fmt.Errorf("unknown agent command: %s", args[1])
			}
		case "models":
			if args[1] != "list" && args[1] != "refresh" && args[1] != "overlay" {
				return fmt.Errorf("unknown models command: %s", args[1])
			}
		case "proxy":
			known := map[string]bool{"start": true, "install": true, "uninstall": true, "restart": true, "validate-http": true, "status": true, "pin": true, "codex-default": true, "prime": true, "endpoint": true}
			if !known[args[1]] {
				return fmt.Errorf("unknown proxy command: %s", args[1])
			}
		}
	}
	return nil
}

func manualHelpInspectionPath(args []string) ([]string, bool) {
	if len(args) == 2 && args[0] == "codex" && (args[1] == "validate" || args[1] == "canary") {
		return []string{"codex", args[1]}, true
	}
	if len(args) == 2 && args[0] == "proxy" && args[1] == "endpoint" {
		return []string{"proxy", "endpoint"}, true
	}
	if len(args) == 0 || !helpRequested(args) {
		return nil, false
	}
	switch args[0] {
	case "refresh":
		return []string{"refresh"}, true
	case "agent":
		return interceptedGroupHelpPath("agent", args[1:], map[string]bool{"install": true, "uninstall": true})
	case "models":
		return modelsHelpInspectionPath(args[1:])
	case "proxy":
		return proxyHelpInspectionPath(args[1:])
	case "codex":
		if len(args) >= 2 && (args[1] == "validate" || args[1] == "canary") && (len(args) == 2 || helpRequested(args[2:])) {
			return []string{"codex", args[1]}, true
		}
	}
	return nil, false
}

func interceptedGroupHelpPath(group string, args []string, leaves map[string]bool) ([]string, bool) {
	if len(args) == 0 {
		return nil, false
	}
	if args[0] == "--help" || args[0] == "-h" {
		return []string{group}, true
	}
	if args[0] == "help" {
		path := append([]string{group}, args[1:]...)
		_, ok := manualHelp(path)
		return path, ok
	}
	if leaves[args[0]] && helpRequested(args[1:]) {
		return []string{group, args[0]}, true
	}
	return nil, false
}

func modelsHelpInspectionPath(args []string) ([]string, bool) {
	if len(args) == 0 {
		return nil, false
	}
	if args[0] == "overlay" {
		if len(args) > 1 && (args[1] == "--help" || args[1] == "-h") {
			return []string{"models", "overlay"}, true
		}
		if len(args) > 1 && args[1] == "help" {
			path := append([]string{"models", "overlay"}, args[2:]...)
			_, ok := manualHelp(path)
			return path, ok
		}
		if len(args) > 1 && (args[1] == "add" || args[1] == "remove" || args[1] == "prune") && helpRequested(args[2:]) {
			return []string{"models", "overlay", args[1]}, true
		}
		return nil, false
	}
	return interceptedGroupHelpPath("models", args, map[string]bool{"list": true, "refresh": true})
}

func proxyHelpInspectionPath(args []string) ([]string, bool) {
	if len(args) == 0 {
		return nil, false
	}
	if args[0] == "prime" {
		return proxyPrimeHelpInspectionPath(args[1:])
	}
	if args[0] == "endpoint" {
		if len(args) == 1 || helpRequested(args[1:]) {
			path := []string{"proxy", "endpoint"}
			if len(args) > 1 && (args[1] == "inspect-legacy" || args[1] == "transition-legacy") {
				path = append(path, args[1])
			}
			return path, true
		}
		return nil, false
	}
	leaves := map[string]bool{
		"start": true, "install": true, "uninstall": true, "restart": true,
		"validate-http": true, "status": true, "pin": true, "codex-default": true,
	}
	return interceptedGroupHelpPath("proxy", args, leaves)
}

func proxyPrimeHelpInspectionPath(args []string) ([]string, bool) {
	if len(args) == 0 {
		return nil, false
	}
	if args[0] == "--help" || args[0] == "-h" {
		return []string{"proxy", "prime"}, true
	}
	if args[0] == "help" {
		path := append([]string{"proxy", "prime"}, args[1:]...)
		_, ok := manualHelp(path)
		return path, ok
	}
	if len(args) == 2 && (args[0] == "status" || args[0] == "enable" || args[0] == "disable") && helpRequested(args[1:]) {
		return []string{"proxy", "prime", args[0]}, true
	}
	return nil, false
}

type errorRecordingWriter struct {
	writer io.Writer
	err    error
}

func (writer *errorRecordingWriter) Write(data []byte) (int, error) {
	count, err := writer.writer.Write(data)
	if err != nil && writer.err == nil {
		writer.err = err
	}
	return count, err
}

func pureInspectionErrorWasRendered(err error) bool {
	var parseError *kong.ParseError
	return errors.As(err, &parseError)
}

type pureInspectionExit int

func parsePureInspection(parser *kong.Kong, args []string, exitCode *int) (err error) {
	defer catchPureInspectionExit(exitCode, &err)
	_, err = parser.Parse(args)
	return err
}

func fatalPureInspection(parser *kong.Kong, parseError error, exitCode *int) (err error) {
	defer catchPureInspectionExit(exitCode, &err)
	parser.FatalIfErrorf(parseError)
	return nil
}

func catchPureInspectionExit(exitCode *int, err *error) {
	recovered := recover()
	if recovered == nil {
		return
	}
	code, ok := recovered.(pureInspectionExit)
	if !ok {
		panic(recovered)
	}
	*exitCode = int(code)
	*err = nil
}

func isInterceptedCommand(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "refresh", "agent", "proxy", "models":
		return true
	case "codex":
		return len(args) > 1 && (args[1] == "validate" || args[1] == "canary")
	default:
		return false
	}
}
