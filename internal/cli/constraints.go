package cli

import (
	"sort"
	"strings"
)

// Only argv combinations belong here. State, inventory and confirmation are
// intentionally left to the semantic owner recorded in parser-coverage.json.
var constraintsByPath = map[string]func(*parser){
	"check":                      uniqueProviders,
	"auth refresh":               uniqueProviders,
	"codex proxy policy apply":   offlineSelector,
	"codex proxy policy show":    offlineSelector,
	"codex proxy session bind":   sessionSelector,
	"codex proxy session digest": sessionSelector,
	"codex proxy session show":   sessionSelector,
	"codex proxy session unbind": sessionSelector,
	"proxy candidate prepare":    candidateCredentialOptions,
}

func (p *parser) validateConstraints() {
	if f := constraintsByPath[p.in.Path]; f != nil {
		f(p)
	}
}
func uniqueProviders(p *parser) {
	seen := map[string]bool{}
	for _, v := range p.values {
		if v.parameter.Name != "providers" {
			continue
		}
		if seen[v.value] {
			p.invalid(v.position, "providers", "duplicates invalid")
		}
		seen[v.value] = true
	}
}
func offlineSelector(p *parser) {
	if p.in.Supplied["state-dir"] && p.in.Supplied["port"] {
		p.conflict("state-dir", "port")
	}
}
func sessionSelector(p *parser) {
	names := []string{}
	for _, name := range []string{"session-id", "session-id-stdin", "digest"} {
		if p.in.Supplied[name] && (name != "session-id-stdin" || p.flag(name)) {
			names = append(names, name)
		}
	}
	if len(names) > 1 {
		p.conflict(names...)
	} else if len(names) == 0 {
		p.invalid(int(^uint(0)>>1), "session selector", "Exactly one selector is required except for list")
	}
}
func candidateCredentialOptions(p *parser) {
	mode := "none"
	if len(p.in.Options["credential-mode"]) > 0 {
		mode = p.in.Options["credential-mode"][0]
	}
	if mode == "none" {
		if p.in.Supplied["credential-manifest"] {
			p.conflict("credential-mode", "credential-manifest")
		}
		if p.flag("confirm-read-only-credentials") {
			p.conflict("credential-mode", "confirm-read-only-credentials")
		}
	} else if mode == "read-only" && !p.in.Supplied["credential-manifest"] {
		p.issue(int(^uint(0)>>1), "missing_option", "Missing required option: --credential-manifest.", false)
	}
	// Required read-only confirmation is a semantic consent precondition (exit 6).
}
func (p *parser) conflict(names ...string) {
	pos := -1
	options := make([]string, len(names))
	for i, n := range names {
		pos = max(pos, p.positions[n])
		options[i] = "--" + n
	}
	sort.Strings(options)
	p.issue(pos, "conflicting_options", "Options "+strings.Join(options, ", ")+" cannot be used together.", true)
}
func specificDiagnostic(path, code, message string) (string, string) {
	if message == "--refresh is only supported by cq check; use --fresh." || message == "--clear cannot be combined with an account." || strings.HasPrefix(message, "This operation uses a fixed 30s deadline;") {
		return code, message
	}
	if code == "invalid_argument" && strings.Contains(message, "Account reference must not be empty") && (strings.Contains(path, " account ") || strings.HasPrefix(path, "codex reset ")) {
		return "account_reference_empty", "Account reference must not be empty."
	}
	if path == "codex proxy fixture create" && code == "invalid_argument" && (strings.HasPrefix(message, "Invalid metadata-json:") || strings.HasPrefix(message, "Invalid content-encoding:")) {
		return "fixture_input_invalid", "The request cannot be converted to a sanitised fixture."
	}
	if routingErrorPaths[path] {
		return "routing_invalid_argument", "Invalid argument: " + strings.TrimSuffix(message, ".") + "."
	}
	if path == "auth refresh" || strings.HasPrefix(path, "models ") {
		return "cli_invalid_usage", "Invalid arguments: " + strings.TrimSuffix(message, ".") + ". Run cq " + path + " --help."
	}
	return code, message
}

var routingErrorPaths = map[string]bool{
	"claude proxy pin clear":       true,
	"claude proxy pin set":         true,
	"claude proxy pin show":        true,
	"codex proxy fallback clear":   true,
	"codex proxy fallback set":     true,
	"codex proxy fallback show":    true,
	"codex proxy lease invalidate": true,
	"codex proxy pin clear":        true,
	"codex proxy pin set":          true,
	"codex proxy pin show":         true,
	"codex proxy policy apply":     true,
	"codex proxy policy show":      true,
	"codex proxy pool rename":      true,
	"codex proxy pool set":         true,
	"codex proxy pool value":       true,
	"codex proxy prime disable":    true,
	"codex proxy prime enable":     true,
	"codex proxy prime status":     true,
	"codex proxy reserve clear":    true,
	"codex proxy reserve disable":  true,
	"codex proxy reserve enable":   true,
	"codex proxy reserve set":      true,
	"codex proxy reserve status":   true,
	"codex proxy reserve windows":  true,
	"codex proxy session bind":     true,
	"codex proxy session digest":   true,
	"codex proxy session list":     true,
	"codex proxy session show":     true,
	"codex proxy session unbind":   true,
	"codex proxy trace":            true,
}
