package cli

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

type aliasSpec struct {
	target      string
	renames     map[string]string
	inject      map[string]string
	duration    string
	conditional bool
}

// Each entry is an explicit migration.json row; there is no prefix guessing.
var aliases = map[string]aliasSpec{
	"check":                                         {target: "check", renames: map[string]string{"-j": "--json", "--refresh": "--fresh", "-r": "--fresh"}},
	"claude login":                                  {target: "claude account login"},
	"claude accounts":                               {target: "claude account list"},
	"claude switch":                                 {target: "claude account activate"},
	"claude remove":                                 {target: "claude account remove"},
	"codex login":                                   {target: "codex account login"},
	"codex accounts":                                {target: "codex account list"},
	"codex switch":                                  {target: "codex account activate"},
	"codex remove":                                  {target: "codex account remove"},
	"gemini accounts":                               {target: "gemini account show"},
	"codex resets list":                             {target: "codex reset list"},
	"codex resets recommend":                        {target: "codex reset recommend"},
	"codex resets use":                              {target: "codex reset use"},
	"refresh":                                       {target: "auth refresh"},
	"agent install":                                 {target: "service install", inject: map[string]string{"component": "token-refresh"}},
	"agent uninstall":                               {target: "service uninstall", inject: map[string]string{"component": "token-refresh"}},
	"proxy install":                                 {target: "service install", inject: map[string]string{"component": "proxy"}},
	"proxy restart":                                 {target: "service restart", inject: map[string]string{"component": "proxy"}},
	"proxy uninstall":                               {target: "service uninstall", inject: map[string]string{"component": "proxy"}},
	"proxy start":                                   {target: "proxy serve"},
	"proxy status":                                  {target: "proxy status", renames: map[string]string{"--instance-state-root": "--state-dir"}, duration: "timeout"},
	"proxy validate-http":                           {target: "codex proxy validate http"},
	"proxy pin claude":                              {target: "claude proxy pin set", conditional: true},
	"proxy pin codex":                               {target: "codex proxy pin set", conditional: true},
	"proxy default codex":                           {target: "codex proxy fallback set", conditional: true},
	"proxy prime status":                            {target: "codex proxy prime status"},
	"proxy prime enable":                            {target: "codex proxy prime enable"},
	"proxy prime disable":                           {target: "codex proxy prime disable"},
	"proxy leases invalidate":                       {target: "codex proxy lease invalidate"},
	"proxy trace":                                   {target: "codex proxy trace"},
	"proxy reserve set":                             {target: "codex proxy reserve set"},
	"proxy reserve disable":                         {target: "codex proxy reserve disable"},
	"proxy reserve enable":                          {target: "codex proxy reserve enable"},
	"proxy reserve clear":                           {target: "codex proxy reserve clear"},
	"proxy reserve status":                          {target: "codex proxy reserve status"},
	"proxy reserve windows":                         {target: "codex proxy reserve windows"},
	"proxy policy initialise":                       {target: "proxy state initialise", renames: map[string]string{"--state-root": "--state-dir"}},
	"proxy policy apply":                            {target: "codex proxy policy apply", renames: map[string]string{"--state-root": "--state-dir"}},
	"proxy policy status":                           {target: "codex proxy policy show", renames: map[string]string{"--state-root": "--state-dir"}},
	"proxy policy pool set":                         {target: "codex proxy pool set"},
	"proxy policy pool rename":                      {target: "codex proxy pool rename"},
	"proxy policy pool value":                       {target: "codex proxy pool value"},
	"proxy policy session bind":                     {target: "codex proxy session bind"},
	"proxy policy session show":                     {target: "codex proxy session show"},
	"proxy policy session list":                     {target: "codex proxy session list"},
	"proxy policy session unbind":                   {target: "codex proxy session unbind"},
	"proxy policy session digest":                   {target: "codex proxy session digest"},
	"proxy endpoint inspect-legacy":                 {target: "codex proxy credential-endpoint legacy inspect"},
	"proxy endpoint transition-legacy prepare":      {target: "codex proxy credential-endpoint legacy prepare"},
	"proxy endpoint transition-legacy resume":       {target: "codex proxy credential-endpoint legacy resume"},
	"proxy endpoint transition-legacy activate":     {target: "codex proxy credential-endpoint legacy activate"},
	"proxy endpoint transition-legacy finalise":     {target: "codex proxy credential-endpoint legacy finalise"},
	"proxy endpoint transition-legacy rollback":     {target: "codex proxy credential-endpoint legacy rollback"},
	"proxy hook codex-stop":                         {target: "codex proxy hook stop"},
	"operation status":                              {target: "proxy operation status"},
	"codex validate capture":                        {target: "codex proxy fixture create", renames: map[string]string{"--metadata": "--metadata-json"}},
	"codex validate http":                           {target: "codex proxy readiness show"},
	"codex validate websocket":                      {target: "codex proxy validate websocket"},
	"codex canary start":                            {target: "codex proxy canary start"},
	"codex canary status":                           {target: "codex proxy canary status"},
	"codex canary stop":                             {target: "codex proxy canary stop"},
	"proxy candidate prepare":                       {target: "proxy candidate prepare", renames: map[string]string{"--instance-state-root": "--state-dir", "--local-token-client-registry": "--client-registry", "--target-release-set": "--release-digest"}, duration: "timeout"},
	"proxy candidate status":                        {target: "proxy candidate status", renames: map[string]string{"--instance-state-root": "--state-dir"}},
	"proxy candidate start":                         {target: "proxy candidate start", renames: map[string]string{"--instance-state-root": "--state-dir"}, duration: "timeout"},
	"proxy candidate stop":                          {target: "proxy candidate stop", renames: map[string]string{"--instance-state-root": "--state-dir"}, duration: "fixed"},
	"proxy candidate client-bearer-barrier refresh": {target: "proxy candidate client-safety refresh", renames: map[string]string{"--instance-state-root": "--state-dir", "--validation-run": "--validation-run-id"}, duration: "timeout"},
	"proxy candidate artifact switch":               {target: "proxy candidate release activate", renames: map[string]string{"--instance-state-root": "--state-dir", "--release-set": "--release-digest", "--validation-run": "--validation-run-id"}, duration: "timeout"},
	"proxy candidate validate-release":              {target: "proxy candidate release validate", renames: map[string]string{"--floor-acceptance-receipt": "--rollback-receipt-digest", "--floor-acceptance-receipt-file": "--rollback-receipt", "--floor-release-bundle": "--rollback-bundle", "--instance-state-root": "--state-dir", "--receipt-out": "--receipt-file", "--validation-run": "--validation-run-id"}},
	"proxy candidate remove":                        {target: "proxy candidate remove", renames: map[string]string{"--instance-state-root": "--state-dir"}, duration: "fixed"},
	"proxy candidate receipt show":                  {target: "proxy candidate receipt show", renames: map[string]string{"--instance-state-root": "--state-dir"}},
}

var compatibilityGroups = map[string]string{
	"agent": "service", "codex resets": "codex reset", "codex validate": "codex proxy", "codex canary": "codex proxy canary",
	"proxy reserve": "codex proxy reserve", "proxy default": "codex proxy fallback", "proxy default codex": "codex proxy fallback",
	"proxy prime": "codex proxy prime", "proxy policy": "codex proxy", "proxy policy pool": "codex proxy pool", "proxy policy session": "codex proxy session",
	"proxy leases": "codex proxy lease", "proxy hook": "codex proxy hook", "proxy endpoint": "codex proxy credential-endpoint legacy",
	"proxy endpoint transition-legacy": "codex proxy credential-endpoint legacy", "proxy candidate client-bearer-barrier": "proxy candidate client-safety",
	"proxy candidate artifact": "proxy candidate release", "operation": "proxy operation", "proxy pin claude": "claude proxy pin", "proxy pin codex": "codex proxy pin",
}

const retiredEndpointCommitPath = "proxy endpoint transition-legacy commit"

const compatibilityPinHelp = "Usage: cq <provider> proxy pin <command>\n\nChoose a provider explicitly:\n  cq claude proxy pin --help\n  cq codex proxy pin --help\n"

func compatibilityHelp(path string) (string, bool) {
	if path == "proxy pin" {
		return compatibilityPinHelp, true
	}
	return "", false
}
func knownParsePath(path string) bool {
	if _, ok := commandSpec(path); ok {
		return true
	}
	if _, ok := aliases[path]; ok {
		return true
	}
	if _, ok := compatibilityGroups[path]; ok {
		return true
	}
	return path == "proxy pin" || path == "operation recover" || path == retiredEndpointCommitPath
}
func isParseLeaf(path string) bool {
	if path == "operation recover" || path == retiredEndpointCommitPath {
		return true
	}
	if _, ok := aliases[path]; ok {
		return true
	}
	s, ok := commandSpec(path)
	return ok && s.Kind == "command"
}
func parseSpec(path string, help bool) (CommandSpec, aliasSpec) {
	if path == retiredEndpointCommitPath {
		// The annex retires commit explicitly; it is never an alias for finalise.
		// Recognise its old options for syntax checks without requiring authority
		// inputs for an operation that cannot run.
		spec, _ := commandSpec("codex proxy credential-endpoint legacy activate")
		spec.Path = path
		spec.Options = append([]ParameterSpec{}, spec.Options...)
		for i := range spec.Options {
			spec.Options[i].Required = false
		}
		return spec, aliasSpec{}
	}

	a := aliases[path]
	if help {
		if target, ok := compatibilityGroups[path]; ok && !a.conditional {
			s, _ := commandSpec(target)
			return s, aliasSpec{target: target}
		}
	}
	if a.target != "" {
		s, _ := commandSpec(a.target)
		s.Options = append([]ParameterSpec{}, s.Options...)
		s.Positionals = append([]ParameterSpec{}, s.Positionals...)
		if a.conditional {
			s.Positionals[0].Required = false
			s.Options = append(s.Options, ParameterSpec{Name: "clear", Type: "boolean"})
		}
		if path == "proxy status" {
			s.Options = append(s.Options, ParameterSpec{Name: "port", Type: "integer", Metavar: "PORT"}, ParameterSpec{Name: "human", Type: "boolean"})
		}
		if path == "operation status" {
			s.Options = append(s.Options, ParameterSpec{Name: "operation-id", Type: "string", Metavar: "OPERATION_ID"})
			s.Positionals = nil
		}
		if path == "proxy candidate artifact switch" {
			s.Options = append(s.Options, ParameterSpec{Name: "role", Type: "enum", Metavar: "ROLE", Choices: []string{"runtime-bundle"}, Required: true})
		}
		return s, a
	}
	if path == "operation recover" {
		return CommandSpec{Path: path, Kind: "command", Options: []ParameterSpec{{Name: "operation-id", Type: "string", Metavar: "OPERATION_ID", Required: true}}}, a
	}
	if path == "proxy pin" {
		return CommandSpec{Path: path, Kind: "command"}, a
	}
	if target, ok := compatibilityGroups[path]; ok {
		return CommandSpec{Path: path, Kind: "group"}, aliasSpec{target: target}
	}
	s, _ := commandSpec(path)
	return s, a
}
func (p *parser) consume(name string) {
	delete(p.in.Options, name)
	delete(p.in.Supplied, name)
}
func (p *parser) finishAlias(helpCommand bool) {
	if p.alias.conditional {
		base := strings.TrimSuffix(p.alias.target, " set")
		selector := p.in.Arguments["account"]
		if len(selector) > 0 && p.flag("clear") {
			p.issue(max(p.positions["account"], p.positions["clear"]), "invalid_argument", "--clear cannot be combined with an account.", true)
		}
		switch {
		case p.flag("help") || helpCommand:
			p.in.Path = base
		case p.flag("clear"):
			p.in.Path = base + " clear"
		case len(selector) > 0:
			p.in.Path = base + " set"
		default:
			p.in.Path = base + " show"
		}
		if p.rawPath == "proxy pin claude" && len(selector) > 0 && uuidPattern.MatchString(selector[0]) {
			p.in.LegacySelector = "claude_uuid"
		}
		p.consume("clear")
	}
	if p.rawPath == "proxy status" {
		if p.in.Supplied["port"] {
			p.in.Path = "proxy health"
			for _, name := range []string{"state-dir", "strict", "human"} {
				if p.in.Supplied[name] {
					p.conflict("port", name)
				}
			}
		}
		if p.flag("human") && p.flag("json") {
			p.conflict("human", "json")
		}
		if p.in.Supplied["human"] {
			p.optionWarning(p.positions["human"], "--human", "--json=false")
		}
		p.consume("human")
	}
	if p.rawPath == "operation status" && p.in.Supplied["operation-id"] {
		p.in.Arguments["operation-id"] = p.in.Options["operation-id"]
		delete(p.in.Options, "operation-id")
	}
	if p.alias.duration == "fixed" && p.in.Supplied["legacy-duration"] {
		value := p.in.Options["legacy-duration"][0]
		d, e := time.ParseDuration(value)
		if e != nil || d != 30*time.Second {
			p.issue(p.positions["legacy-duration"], "invalid_argument", "This operation uses a fixed 30s deadline; omit the legacy duration or supply 30s.", true)
		}
		p.consume("legacy-duration")
	}
}
func (p *parser) optionWarning(position int, old, new string) {
	message := "Deprecated option " + old + "; use " + new + "."
	for _, w := range p.optionWarnings {
		if w.diagnostic.Message == message {
			return
		}
	}
	p.optionWarnings = append(p.optionWarnings, warningPosition{position, Diagnostic{Code: "deprecated_option", Message: message}})
}
func (p *parser) aliasWarnings() {
	sort.SliceStable(p.optionWarnings, func(i, j int) bool { return p.optionWarnings[i].position < p.optionWarnings[j].position })
	for _, w := range p.optionWarnings {
		p.in.Warnings = append(p.in.Warnings, w.diagnostic)
	}
	if p.rawPath != "" && p.rawPath != p.in.Path {
		p.in.Warnings = []Diagnostic{{Code: "deprecated_alias", Message: "Deprecated syntax; use cq " + p.in.Path + "."}}
		if p.rawPath == "proxy policy" {
			p.in.Warnings = append(p.in.Warnings, Diagnostic{Code: "legacy_group_split", Message: "Shared state initialisation moved to cq proxy state initialise."})
		}
	}
	if p.rawPath == "operation recover" || p.rawPath == "proxy pin" {
		p.in.Warnings = []Diagnostic{}
	}
}

// The old capture route accepted a compaction object. Canonical explicit
// metadata accepts only a string. Decode strictly before this one translation.
func normaliseLegacyMetadata(value string) string {
	if len(value) > 65536 {
		return value
	}
	var raw map[string]json.RawMessage
	if strictJSON(value, &raw) != nil {
		return value
	}
	field, ok := raw["compaction"]
	if !ok || len(field) == 0 || field[0] != '{' {
		return value
	}
	var phase struct {
		Phase string `json:"phase"`
	}
	if strictJSON(string(field), &phase) != nil {
		return value
	}
	raw["compaction"], _ = json.Marshal(phase.Phase)
	b, e := json.Marshal(raw)
	if e != nil {
		return value
	}
	return string(b)
}
