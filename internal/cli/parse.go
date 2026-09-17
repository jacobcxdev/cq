package cli

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ParseError preserves the retirement exit as well as ordinary syntax failures.
type ParseError struct {
	Path       string
	ExitCode   int
	Diagnostic Diagnostic
}

func (e *ParseError) Error() string { return e.Diagnostic.Message }

type argvToken struct {
	text     string
	position int
	literal  bool
}
type parseIssue struct {
	position      int
	code, message string
	value         bool
}
type suppliedValue struct {
	parameter ParameterSpec
	value     string
	position  int
}

type warningPosition struct {
	position   int
	diagnostic Diagnostic
}

type parser struct {
	optionWarnings []warningPosition
	in             Invocation
	issues         []parseIssue
	values         []suppliedValue
	positions      map[string]int
	inspect        bool
	helpCommand    bool
	rawPath        string
	legacyGlobals  []argvToken
	alias          aliasSpec
}

// Parse inspects only argv and immutable catalogue data. It never resolves files,
// credentials, environment defaults, account identities or semantic consent.
func Parse(argv []string) (Invocation, *ParseError) {
	p := parser{in: Invocation{Arguments: map[string][]string{}, Options: map[string][]string{}, Supplied: map[string]bool{}, Presentation: "run", Warnings: []Diagnostic{}}, positions: map[string]int{}}
	tokens := p.globals(argv)
	p.inspect = p.flag("help") || p.flag("version")
	raw, tail, helpCommand := p.resolve(tokens)
	if raw == "" && len(p.legacyGlobals) > 0 && len(tokens) == 0 {
		raw = "check"
	}
	p.rawPath = raw
	p.helpCommand = helpCommand
	if helpCommand {
		p.inspect = true
		p.in.Presentation = "help"
	}
	p.in.Path = raw
	if raw == "" && len(tokens) == 0 && !p.inspect && !helpCommand {
		raw = "check"
		p.in.Path = raw
		p.rawPath = raw
	}
	if raw == "" && (p.flag("help") || helpCommand) {
		p.in.Presentation = "help"
	}
	spec, alias := parseSpec(raw, p.flag("help") || helpCommand)
	p.alias = alias
	if alias.target != "" {
		p.in.Path = alias.target
	}
	if raw == "" {
		spec = CommandSpec{Kind: "group"}
	}
	for name, value := range alias.inject {
		p.set(ParameterSpec{Name: name}, value, -1, false)
	}
	tail = append(append([]argvToken{}, tail...), p.legacyGlobals...)
	sort.SliceStable(tail, func(i, j int) bool { return tail[i].position < tail[j].position })

	p.scan(spec, tail)
	p.finishAlias(helpCommand)
	if p.flag("help") && p.flag("version") {
		p.issue(max(p.positions["help"], p.positions["version"]), "conflicting_options", "Options --help, --version cannot be used together.", false)
	}
	if p.flag("help") || helpCommand {
		p.in.Presentation = "help"
	} else if p.flag("version") || p.in.Path == "version" {
		p.in.Presentation = "version"
		p.inspect = true
	} else if spec.Kind == "group" {
		p.in.Presentation = "help"
	}
	p.in.JSON = p.flag("json")
	// Validation is delayed until inspection controls and the complete canonical
	// destination are known. Errors still retain their original argv position.
	if !p.inspect {
		for _, v := range p.values {
			if constraint := validateValue(p.in.Path, v.parameter, v.value, p.in.LegacySelector); constraint != "" {
				p.invalid(v.position, v.parameter.Name, catalogueConstraint(p.in.Path, v.parameter.Name, constraint))
			}
		}
		p.validateConstraints()
		for _, v := range spec.Positionals {
			if v.Required && len(p.in.Arguments[v.Name]) == 0 {
				p.issue(len(argv)+1, "missing_argument", "Missing required argument: "+metavar(v)+".", false)
			}
		}
		for _, v := range spec.Options {
			if v.Required && v.Type != "boolean" && len(p.in.Options[v.Name]) == 0 {
				p.issue(len(argv)+1, "missing_option", "Missing required option: --"+v.Name+".", false)
			}
		}
	}
	if actual, ok := commandSpec(p.in.Path); ok {
		spec = actual
	}
	p.defaults(spec)
	p.consume("role")
	p.consume("clear")
	p.consume("human")
	p.consume("legacy-duration")
	p.aliasWarnings()
	if e := p.firstError(); e != nil {
		return p.in, e
	}
	if raw == "operation recover" {
		return p.in, &ParseError{Path: raw, ExitCode: 4, Diagnostic: Diagnostic{Code: "operation_recovery_unavailable", Message: "Active operation recovery is unavailable; use cq proxy operation status OPERATION_ID to inspect retained state."}}
	}
	if raw == "proxy pin" && p.in.Presentation != "help" {
		return p.in, &ParseError{Path: raw, ExitCode: 2, Diagnostic: Diagnostic{Code: "provider_required", Message: "Specify a provider: use cq claude proxy pin show or cq codex proxy pin show."}}
	}
	return p.in, nil
}

func (p *parser) globals(argv []string) []argvToken {
	tokens := make([]argvToken, 0, len(argv))
	literal := false
	for i, s := range argv {
		if !literal && s == "--" {
			literal = true
			continue
		}
		name, value, equals := splitOption(s)
		if !literal && (name == "--refresh" || name == "-r") {
			p.legacyGlobals = append(p.legacyGlobals, argvToken{s, i, false})
			continue
		}
		if !literal {
			if g, ok := globalParameter(name); ok {
				if !equals {
					value = "true"
				}
				p.set(g, value, i, false)
				if value != "true" && value != "false" {
					p.issue(i, "invalid_argument", "Invalid "+g.Name+": expected true or false.", false)
				}
				continue
			}
		}
		tokens = append(tokens, argvToken{s, i, literal})
	}
	return tokens
}

func (p *parser) resolve(tokens []argvToken) (string, []argvToken, bool) {
	path := ""
	help := false
	for i, t := range tokens {
		if t.text == "help" && !help {
			help = true
			continue
		}
		child := t.text
		if path != "" {
			child = path + " " + t.text
		}
		if knownParsePath(child) {
			path = child
			if isParseLeaf(path) {
				return path, tokens[i+1:], help
			}
			continue
		}
		if strings.HasPrefix(t.text, "-") && !t.literal {
			if path == "" {
				return "check", tokens[i:], help
			}
			p.in.Path = path
			name, _, _ := splitOption(t.text)
			p.issue(t.position, "unknown_option", unknownOption(name, path), false)
			return path, nil, help
		}
		p.in.Path = path
		p.issue(t.position, "unknown_command", "Unknown command: "+safeText(t.text)+". Run cq help.", false)
		return path, nil, help
	}
	return path, nil, help
}

func (p *parser) scan(spec CommandSpec, tokens []argvToken) {
	positional := 0
	for i := 0; i < len(tokens); i++ {
		t := tokens[i]
		if !t.literal && strings.HasPrefix(t.text, "-") && t.text != "-" {
			name, value, equals := splitOption(t.text)
			if name == "--refresh" || name == "-r" {
				if p.in.Path != "check" {
					p.issue(t.position, "invalid_argument", "--refresh is only supported by cq check; use --fresh.", false)
					continue
				}
			}
			canonical := name
			if rename, ok := p.alias.renames[name]; ok {
				canonical = rename
			}
			if p.in.Path == "check" && (name == "--refresh" || name == "-r") {
				canonical = "--fresh"
			}
			option, ok := findOption(spec.Options, canonical)
			if !ok {
				p.issue(t.position, "unknown_option", unknownOption(name, p.in.Path), false)
				continue
			}
			if option.Type == "boolean" {
				if !equals {
					value = "true"
				}
			} else if !equals {
				// Globals were removed, so adjacency is checked against the original argv.
				if i+1 >= len(tokens) || tokens[i+1].position != t.position+1 || tokens[i+1].literal || strings.HasPrefix(tokens[i+1].text, "-") {
					p.issue(t.position, "missing_option_value", "Option --"+option.Name+" requires "+metavar(option)+".", false)
					continue
				}
				i++
				value = tokens[i].text
			}
			if canonical != name {
				p.optionWarning(t.position, name, canonical)
			}
			if strings.HasPrefix(p.in.Path, "models ") && option.Name == "provider" && value == "anthropic" {
				value = "claude"
				p.optionWarning(t.position, "--provider anthropic", "--provider claude")
			}
			if p.in.Path == "codex proxy reserve set" && option.Name == "window" && utf8.ValidString(value) {
				value = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(value)), "_", "-")
			}
			if p.rawPath == "codex validate capture" && option.Name == "content-encoding" && value == "" {
				value = "auto"
			}
			if p.rawPath == "codex validate capture" && option.Name == "metadata-json" {
				value = normaliseLegacyMetadata(value)
			}
			p.set(option, value, t.position, false)
			continue
		}
		if p.helpCommand {
			p.issue(t.position, "unknown_command", "Unknown command: "+safeText(t.text)+". Run cq help.", false)
			continue
		}
		if positional < len(spec.Positionals) {
			parameter := spec.Positionals[positional]
			p.set(parameter, t.text, t.position, true)
			if !parameter.Repeatable {
				positional++
			}
		} else if p.alias.duration != "" {
			if p.alias.duration == "fixed" {
				p.set(ParameterSpec{Name: "legacy-duration", Type: "duration"}, t.text, t.position, false)
			} else {
				p.set(ParameterSpec{Name: "timeout", Type: "duration", Metavar: "DURATION"}, t.text, t.position, false)
			}
		} else {
			p.issue(t.position, "unexpected_argument", "Unexpected argument: "+safeText(t.text)+".", false)
		}
	}
}

func (p *parser) set(v ParameterSpec, value string, position int, positional bool) {
	if p.in.Supplied[v.Name] && !v.Repeatable {
		p.issue(position, "duplicate_option", "Option --"+v.Name+" may be specified only once.", false)
		return
	}
	p.in.Supplied[v.Name] = true
	p.positions[v.Name] = position
	if positional {
		p.in.Arguments[v.Name] = append(p.in.Arguments[v.Name], value)
	} else {
		p.in.Options[v.Name] = append(p.in.Options[v.Name], value)
	}
	p.values = append(p.values, suppliedValue{v, value, position})
}
func (p *parser) defaults(spec CommandSpec) {
	for _, g := range globalOptions {
		if !p.in.Supplied[g.Name] && g.Default != nil {
			p.in.Options[g.Name] = append([]string{}, g.Default...)
		}
	}
	for _, o := range spec.Options {
		if !p.in.Supplied[o.Name] && o.Default != nil {
			p.in.Options[o.Name] = append([]string{}, o.Default...)
		}
	}
	for _, a := range spec.Positionals {
		if !p.in.Supplied[a.Name] && a.Default != nil {
			p.in.Arguments[a.Name] = append([]string{}, a.Default...)
		}
	}
}
func (p *parser) flag(name string) bool {
	return len(p.in.Options[name]) > 0 && p.in.Options[name][0] == "true"
}
func (p *parser) issue(position int, code, message string, value bool) {
	p.issues = append(p.issues, parseIssue{position, code, message, value})
}
func (p *parser) invalid(position int, name, constraint string) {
	p.issue(position, "invalid_argument", "Invalid "+name+": "+strings.TrimSuffix(constraint, ".")+".", true)
}
func (p *parser) firstError() *ParseError {
	if len(p.issues) == 0 {
		return nil
	}
	sort.SliceStable(p.issues, func(i, j int) bool { return p.issues[i].position < p.issues[j].position })
	for _, issue := range p.issues {
		if p.inspect && issue.value {
			continue
		}
		message := issue.message
		if issue.code == "unknown_option" {
			for _, old := range []string{p.rawPath, p.alias.target} {
				if old != "" && old != p.in.Path {
					message = strings.ReplaceAll(message, "Run cq "+old+" --help.", "Run cq "+p.in.Path+" --help.")
				}
			}
		}
		code, message := specificDiagnostic(p.in.Path, issue.code, message)
		return &ParseError{Path: p.in.Path, ExitCode: 2, Diagnostic: Diagnostic{code, message}}
	}
	return nil
}
func splitOption(s string) (string, string, bool) {
	name, value, ok := strings.Cut(s, "=")
	return name, value, ok
}
func globalParameter(name string) (ParameterSpec, bool) {
	for _, p := range globalOptions {
		if name == "--"+p.Name || name == "-"+p.Short {
			return p, true
		}
	}
	return ParameterSpec{}, false
}
func findOption(options []ParameterSpec, name string) (ParameterSpec, bool) {
	for _, p := range options {
		if name == "--"+p.Name || (p.Short != "" && name == "-"+p.Short) {
			return p, true
		}
	}
	return ParameterSpec{}, false
}
func commandSpec(path string) (CommandSpec, bool) {
	for _, c := range catalogue {
		if c.Path == path {
			return c, true
		}
	}
	return CommandSpec{}, false
}
func metavar(p ParameterSpec) string {
	if p.Metavar != "" {
		return p.Metavar
	}
	return strings.ToUpper(p.Name)
}
func unknownOption(option, path string) string {
	suffix := ""
	if path != "" {
		suffix = path + " "
	}
	return "Unknown option: " + safeText(option) + ". Run cq " + suffix + "--help."
}
func safeText(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) {
			fmt.Fprintf(&b, "\\u%04x", r)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
