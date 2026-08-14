package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/alecthomas/kong"
)

var manualHelpByPath = map[string]string{
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
func runPureGlobalInspection(args []string, stdout, stderr io.Writer) (bool, error) {
	if len(args) != 1 {
		return false, nil
	}
	switch args[0] {
	case "--version", "-v":
		_, err := fmt.Fprintln(stdout, version)
		return true, err
	case "--help", "-h", "help":
		var cli CLI
		options := append(cliKongOptions(),
			kong.Writers(stdout, stderr),
			kong.Exit(func(int) {}),
		)
		parser, err := kong.New(&cli, options...)
		if err != nil {
			return true, err
		}
		_, _ = parser.Parse([]string{"--help"})
		return true, nil
	default:
		return false, nil
	}
}
