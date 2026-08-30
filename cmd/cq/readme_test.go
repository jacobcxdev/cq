package main

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

type commandDispatcherSpec struct {
	file     string
	function string
	selector string
	prefix   string
}

type commandDispatcherKey struct {
	file     string
	function string
	selector string
}

type commandDispatcherCoverage struct {
	commandDispatcherKey
	commands []string
}

type sourceCommandDispatcher struct {
	commandDispatcherKey
	commands []string
}

var commandDispatcherSpecs = []commandDispatcherSpec{
	{"main.go", "main", "os.Args[1]", ""},
	{"proxy_commands.go", "ClassifyProxyCommand", "argv[0]", ""},
	{"proxy_commands.go", "ClassifyProxyCommand", "argv[1]", "codex"},
	{"proxy_commands.go", "classifyAgentAuthority", "argv[1]", "agent"},
	{"proxy_commands.go", "classifyAgentAuthority", "argv[2]", "agent"},
	{"proxy_commands.go", "classifyProxyReadAuthority", "argv[1]", "proxy"},
	{"proxy_commands.go", "classifyModelsAuthority", "argv[1]", "models"},
	{"proxy_commands.go", "classifyModelsAuthority", "argv[2]", "models overlay"},
	{"proxy_commands.go", "classifyCodexAuxiliaryAuthority", "argv[1]", "codex"},
	{"proxy_commands.go", "classifyCodexAuxiliaryAuthority", "argv[2]", "codex validate"},
	{"main.go", "runAgent", "args[0]", "agent"},
	{"proxy.go", "runProxy", "args[0]", "proxy"},
	{"proxy.go", "runProxyDefault", "args[0]", "proxy default"},
	{"proxy.go", "runProxyPrime", "args[0]", "proxy prime"},
	{"proxy.go", "runProxyPinWithDependencies", "args[0]", "proxy pin"},
	{"models.go", "runModels", "args[0]", "models"},
	{"models.go", "runModelsOverlay", "args[0]", "models overlay"},
	{"codex_validate.go", "runCodexValidate", "args[0]", "codex validate"},
	{"codex_canary.go", "runCodexCanary", "command", "codex canary"},
	{"proxy_endpoint_maintenance.go", "runProxyEndpointWithDependencies", "args[0]", "proxy endpoint"},
	{"proxy_endpoint_maintenance.go", "transitionLegacyEndpointCommand", "opts.action", "proxy endpoint transition-legacy"},
	{"proxy_policy.go", "runProxyPolicyWithDependencies", "command", "proxy policy"},
	{"proxy_policy.go", "runProxyPolicyPool", "args[0]", "proxy policy pool"},
	{"proxy_policy.go", "runProxyPolicySession", "command", "proxy policy session"},
	{"proxy_rescue.go", "runProxyRescueWithDependencies", "args[0]", "proxy rescue"},
	{"proxy_commands.go", "classifyOperatorRecoveryAuthority", "argv[1]", "operation"},
	{"proxy_commands.go", "classifyCandidateAuthority", "argv[2]", "proxy candidate"},
	{"proxy_commands.go", "classifyCandidateReceiptAuthority", "argv[3]", "proxy candidate receipt"},
	{"proxy_commands.go", "parseCandidateBarrierArguments", "argv[0]", "proxy candidate client-bearer-barrier"},
	{"proxy_commands.go", "parseCandidateArtifactSwitchArguments", "argv[0]", "proxy candidate artifact"},
	{"proxy_codex_hook.go", "runProxyHook", "args[0]", "proxy hook"},
	{"proxy_commands.go", "validProxyRescueArguments", "argv[0]", "proxy rescue"},
	{"proxy_endpoint_maintenance.go", "isReadOnlyLegacyEndpointInspectCommand", "args[2]", "proxy endpoint"},
	{"proxy_endpoint_maintenance.go", "parseLegacyEndpointTransitionOptions", "opts.action", "proxy endpoint transition-legacy"},
	{"help.go", "runPureGlobalInspectionWithTarget", "args[0]", ""},
	{"help.go", "runPureGlobalInspectionWithTarget", "args[1]", "proxy"},
	{"help.go", "classifyInterceptedInspection", "args[0]", ""},
	{"help.go", "classifyInterceptedInspection", "args[1]", "proxy"},
	{"help.go", "validateInterceptedLexicalGrammar", "args[0]", ""},
	{"help.go", "validateProxyLexicalGrammar", "args[0]", "proxy"},
	{"help.go", "validateProxyEndpointLexicalGrammar", "args[0]", "proxy endpoint"},
	{"help.go", "validateModelsLexicalGrammar", "args[0]", "models"},
	{"help.go", "validateModelsLexicalGrammar", "args[1]", "models overlay"},
	{"help.go", "validateCodexValidationLexicalGrammar", "args[0]", "codex validate"},
	{"help.go", "validateCodexCanaryLexicalGrammar", "args[0]", "codex canary"},
	{"help.go", "interceptedZeroArgumentUsage", "args[0]", ""},
	{"help.go", "manualHelpInspectionPath", "args[0]", ""},
	{"help.go", "modelsHelpInspectionPath", "args[0]", "models"},
	{"help.go", "modelsHelpInspectionPath", "args[1]", "models overlay"},
	{"help.go", "proxyHelpInspectionPath", "args[0]", "proxy"},
	{"help.go", "proxyPrimeHelpInspectionPath", "args[0]", "proxy prime"},
	{"help.go", "isInterceptedCommand", "args[0]", ""},
	{"help.go", "isInterceptedCommand", "args[1]", "codex"},
}

var commandDispatcherCoverageOnly = []commandDispatcherCoverage{
	{commandDispatcherKey{"help.go", "interceptedZeroArgumentUsage", "args[1]"}, []string{"overlay", "prime"}},
	{commandDispatcherKey{"help.go", "manualHelpInspectionPath", "args[1]"}, []string{"canary", "endpoint", "resets", "validate"}},
	{commandDispatcherKey{"help.go", "manualHelpInspectionPath", "args[2]"}, []string{"list", "recommend", "use"}},
	{commandDispatcherKey{"help.go", "manualUsageInspectionError", "args[0]"}, []string{"agent", "models", "proxy", "service"}},
	{commandDispatcherKey{"help.go", "manualUsageInspectionError", "args[1]"}, []string{"install", "list", "overlay", "prime", "refresh", "restart", "status", "uninstall"}},
	{commandDispatcherKey{"help.go", "proxyHelpInspectionPath", "args[1]"}, []string{"artifact", "client-bearer-barrier", "codex", "inspect-legacy", "receipt", "transition-legacy"}},
	{commandDispatcherKey{"help.go", "proxyHelpInspectionPath", "args[2]"}, []string{"refresh", "show", "switch"}},
	{commandDispatcherKey{"help.go", "validateInterceptedLexicalGrammar", "args[1]"}, []string{"canary", "install", "uninstall", "validate"}},
	{commandDispatcherKey{"help.go", "validateProxyLexicalGrammar", "args[1]"}, []string{"codex", "codex-stop", "disable", "enable", "status"}},
	{commandDispatcherKey{"proxy_candidate_runtime.go", "isCandidateRuntimeCommand", "args[0]"}, []string{"proxy"}},
	{commandDispatcherKey{"proxy_candidate_runtime.go", "isCandidateRuntimeCommand", "args[1]"}, []string{"candidate"}},
	{commandDispatcherKey{"proxy_commands.go", "classifyCandidateReceiptAuthority", "argv[1]"}, []string{"candidate"}},
	{commandDispatcherKey{"proxy_commands.go", "classifyCandidateReceiptAuthority", "argv[2]"}, []string{"receipt"}},
	{commandDispatcherKey{"proxy_commands.go", "classifyProxyReadAuthority", "argv[2]"}, []string{"status"}},
	{commandDispatcherKey{"proxy_endpoint_maintenance.go", "isReadOnlyLegacyEndpointInspectCommand", "args[0]"}, []string{"proxy"}},
	{commandDispatcherKey{"proxy_endpoint_maintenance.go", "isReadOnlyLegacyEndpointInspectCommand", "args[1]"}, []string{"endpoint"}},
}

func TestREADMEListsEveryPublicCommandPath(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	readme := string(body)
	want := documentedPublicCommandPaths(t)
	for _, path := range want {
		if !strings.Contains(readme, "cq "+path) {
			t.Errorf("README missing public command path %q", path)
		}
	}
	if got := readmePublicCommandIndex(t, readme); !reflect.DeepEqual(got, want) {
		t.Errorf("README public command index drifted\ngot:  %q\nwant: %q", got, want)
	}
	for _, invocation := range []string{"cq --json", "cq --refresh", "cq --version"} {
		if !strings.Contains(readme, invocation) {
			t.Errorf("README missing global invocation %q", invocation)
		}
	}
}

func TestREADMEDocumentsCompleteInstallationParity(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	readme := string(body)
	normalised := strings.Join(strings.Fields(readme), " ")
	for _, required := range []string{
		"brew install --cask jacobcxdev/tap/cq",
		"winget install jacobcxdev.cq",
		"go run github.com/jacobcxdev/cq/cmd/cq-install@latest",
		"No manual post-install command is required",
		"brew services stop cq",
		"brew uninstall --formula cq",
		"brew uninstall --cask cq",
		"winget uninstall jacobcxdev.cq",
		"go run github.com/jacobcxdev/cq/cmd/cq-install@latest uninstall",
		"configuration, credentials, cache, history, and logs remain",
		"functional systemd user manager",
		"current Windows user without administrator access",
		"## Development and portable binaries",
		"go install github.com/jacobcxdev/cq/cmd/cq@latest",
		"does not install or manage services",
		"headroom-ai",
	} {
		if !strings.Contains(normalised, required) {
			t.Errorf("README missing installation contract %q", required)
		}
	}
	for _, obsolete := range []string{
		"brew install jacobcxdev/tap/cq\n",
		"brew services start cq            # Optional local proxy service",
	} {
		if strings.Contains(readme, obsolete) {
			t.Errorf("README retains obsolete installation instruction %q", obsolete)
		}
	}
}

func TestCommandDispatcherRosterCoversSource(t *testing.T) {
	covered := make(map[commandDispatcherKey]*commandDispatcherCoverage, len(commandDispatcherSpecs)+len(commandDispatcherCoverageOnly))
	for _, spec := range commandDispatcherSpecs {
		covered[commandDispatcherKey{spec.file, spec.function, spec.selector}] = nil
	}
	for index := range commandDispatcherCoverageOnly {
		entry := &commandDispatcherCoverageOnly[index]
		covered[entry.commandDispatcherKey] = entry
	}
	seen := make(map[commandDispatcherKey]bool, len(covered))
	for _, source := range sourceCommandDispatchers(t) {
		coverage, ok := covered[source.commandDispatcherKey]
		if !ok {
			t.Errorf("command dispatcher %s:%s selector %s with commands %q is absent from README source roster", source.file, source.function, source.selector, source.commands)
			continue
		}
		seen[source.commandDispatcherKey] = true
		if coverage != nil && !reflect.DeepEqual(source.commands, coverage.commands) {
			t.Errorf("internal command dispatcher %s:%s selector %s drifted\ngot:  %q\nwant: %q", source.file, source.function, source.selector, source.commands, coverage.commands)
		}
	}
	for _, coverage := range commandDispatcherCoverageOnly {
		if !seen[coverage.commandDispatcherKey] {
			t.Errorf("internal command dispatcher %s:%s selector %s no longer exists", coverage.file, coverage.function, coverage.selector)
		}
	}
}

func documentedPublicCommandPaths(t *testing.T) []string {
	t.Helper()
	paths := make(map[string]struct{})
	for _, path := range publicCommandPathsFromSource(t) {
		paths[path] = struct{}{}
	}
	for path := range manualHelpByPath {
		if path != "" {
			paths[path] = struct{}{}
		}
	}
	result := make([]string, 0, len(paths))
	for path := range paths {
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}

func readmePublicCommandIndex(t *testing.T, readme string) []string {
	t.Helper()
	const start = "<!-- public-command-index:start -->"
	const end = "<!-- public-command-index:end -->"
	startIndex := strings.Index(readme, start)
	endIndex := strings.Index(readme, end)
	if startIndex < 0 || endIndex <= startIndex {
		t.Fatal("README public command index markers missing")
	}
	var result []string
	for _, line := range strings.Split(readme[startIndex+len(start):endIndex], "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "cq ") {
			result = append(result, strings.TrimPrefix(line, "cq "))
		}
	}
	sort.Strings(result)
	return result
}

func publicCommandPathsFromSource(t *testing.T) []string {
	t.Helper()
	paths := make(map[string]struct{})
	for _, path := range kongCommandPaths(reflect.TypeOf(CLI{}), nil) {
		paths[path] = struct{}{}
	}
	for _, spec := range commandDispatcherSpecs {
		for _, command := range stringCommandsForSelector(t, spec.file, spec.function, spec.selector) {
			path := strings.TrimSpace(spec.prefix + " " + command)
			paths[path] = struct{}{}
		}
	}
	result := make([]string, 0, len(paths))
	for path := range paths {
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}

func kongCommandPaths(typ reflect.Type, prefix []string) []string {
	var paths []string
	for index := 0; index < typ.NumField(); index++ {
		field := typ.Field(index)
		if _, ok := field.Tag.Lookup("cmd"); !ok {
			continue
		}
		name := field.Tag.Get("name")
		if name == "" {
			name = kebabName(field.Name)
		}
		path := append(append([]string(nil), prefix...), name)
		if hasKongCommands(field.Type) {
			paths = append(paths, kongCommandPaths(field.Type, path)...)
		} else {
			paths = append(paths, strings.Join(path, " "))
		}
	}
	return paths
}

func hasKongCommands(typ reflect.Type) bool {
	for index := 0; index < typ.NumField(); index++ {
		if _, ok := typ.Field(index).Tag.Lookup("cmd"); ok {
			return true
		}
	}
	return false
}

func kebabName(name string) string {
	runes := []rune(name)
	var result strings.Builder
	for index, current := range runes {
		if unicode.IsUpper(current) && index > 0 && (unicode.IsLower(runes[index-1]) || (index+1 < len(runes) && unicode.IsLower(runes[index+1]))) {
			result.WriteByte('-')
		}
		result.WriteRune(unicode.ToLower(current))
	}
	return result.String()
}

func stringCommandsForSelector(t *testing.T, file, function, selector string) []string {
	t.Helper()
	fset := token.NewFileSet()
	source, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var declaration *ast.FuncDecl
	for _, candidate := range source.Decls {
		if functionDeclaration, ok := candidate.(*ast.FuncDecl); ok && functionDeclaration.Name.Name == function {
			declaration = functionDeclaration
			break
		}
	}
	if declaration == nil {
		t.Fatalf("function %s missing from %s", function, file)
	}
	commands := make(map[string]struct{})
	ast.Inspect(declaration.Body, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.SwitchStmt:
			if expressionText(fset, value.Tag) != selector {
				return true
			}
			for _, statement := range value.Body.List {
				clause := statement.(*ast.CaseClause)
				for _, expression := range clause.List {
					if command, ok := stringLiteral(expression); ok && publicCommandLiteral(command) {
						commands[command] = struct{}{}
					}
				}
			}
		case *ast.BinaryExpr:
			if value.Op != token.EQL && value.Op != token.NEQ {
				return true
			}
			if expressionText(fset, value.X) == selector {
				if command, ok := stringLiteral(value.Y); ok && publicCommandLiteral(command) {
					commands[command] = struct{}{}
				}
			} else if expressionText(fset, value.Y) == selector {
				if command, ok := stringLiteral(value.X); ok && publicCommandLiteral(command) {
					commands[command] = struct{}{}
				}
			}
		}
		return true
	})
	if len(commands) == 0 {
		t.Fatalf("no commands found for %s selector %s", function, selector)
	}
	result := make([]string, 0, len(commands))
	for command := range commands {
		result = append(result, command)
	}
	sort.Strings(result)
	return result
}

func expressionText(fset *token.FileSet, expression ast.Expr) string {
	var result bytes.Buffer
	if err := format.Node(&result, fset, expression); err != nil {
		return ""
	}
	return result.String()
}

func stringLiteral(expression ast.Expr) (string, bool) {
	literal, ok := expression.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(literal.Value)
	return value, err == nil
}

func sourceCommandDispatchers(t *testing.T) []sourceCommandDispatcher {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	selectors := make(map[commandDispatcherKey]struct{})
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		source, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, candidate := range source.Decls {
			declaration, ok := candidate.(*ast.FuncDecl)
			if !ok || declaration.Body == nil {
				continue
			}
			ast.Inspect(declaration.Body, func(node ast.Node) bool {
				switch value := node.(type) {
				case *ast.SwitchStmt:
					selector := expressionText(fset, value.Tag)
					if !commandSelector(selector) {
						return true
					}
					for _, statement := range value.Body.List {
						for _, expression := range statement.(*ast.CaseClause).List {
							if command, ok := stringLiteral(expression); ok && publicCommandLiteral(command) {
								selectors[commandDispatcherKey{file, declaration.Name.Name, selector}] = struct{}{}
							}
						}
					}
				case *ast.BinaryExpr:
					if value.Op != token.EQL && value.Op != token.NEQ {
						return true
					}
					if selector := expressionText(fset, value.X); commandSelector(selector) {
						if command, ok := stringLiteral(value.Y); ok && publicCommandLiteral(command) {
							selectors[commandDispatcherKey{file, declaration.Name.Name, selector}] = struct{}{}
						}
					} else if selector := expressionText(fset, value.Y); commandSelector(selector) {
						if command, ok := stringLiteral(value.X); ok && publicCommandLiteral(command) {
							selectors[commandDispatcherKey{file, declaration.Name.Name, selector}] = struct{}{}
						}
					}
				}
				return true
			})
		}
	}
	result := make([]sourceCommandDispatcher, 0, len(selectors))
	for selector := range selectors {
		commands := stringCommandsForSelector(t, selector.file, selector.function, selector.selector)
		result = append(result, sourceCommandDispatcher{selector, commands})
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].file != result[right].file {
			return result[left].file < result[right].file
		}
		if result[left].function != result[right].function {
			return result[left].function < result[right].function
		}
		return result[left].selector < result[right].selector
	})
	return result
}

func commandSelector(selector string) bool {
	switch selector {
	case "args[0]", "args[1]", "args[2]", "args[3]", "argv[0]", "argv[1]", "argv[2]", "argv[3]", "os.Args[1]", "command", "opts.action":
		return true
	default:
		return false
	}
}

func publicCommandLiteral(command string) bool {
	return command != "" && command != "help" && !strings.HasPrefix(command, "-") && !strings.HasPrefix(command, "_") && !strings.Contains(command, " ")
}
