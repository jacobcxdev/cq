package main

import (
	"fmt"
	"io"
	"strings"
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
  proxy pin           Pin Claude proxy routing
`,
	"proxy start": `Usage: cq proxy start [--port PORT]

Start local Claude and Codex proxy.

Options:
  --port PORT         Override configured listen port for this run
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
	"proxy pin": `Usage: cq proxy pin [--clear | <email-or-account-uuid>]

Pin Claude proxy routing to a specific account, show the current pin, or clear it.

Examples:
  cq proxy pin
  cq proxy pin user@example.com
  cq proxy pin 550e8400-e29b-41d4-a716-446655440000
  cq proxy pin --clear
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
