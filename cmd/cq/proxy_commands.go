package main

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

type CommandDeadlineV1 struct {
	Total   time.Duration
	Forward time.Duration
	Reserve time.Duration
}

type OrdinaryCommandAuthorityV1 struct {
	Catalogue   string
	Row         string
	Terminating bool
	Arguments   any
	Deadline    CommandDeadlineV1
	IgnoredTail []string
}

type OrdinaryCheckArgumentsV1 struct {
	Providers []string
	JSON      bool
	Refresh   bool
	Cache     CheckCachePolicyV1
}

type OrdinaryLoginArgumentsV1 struct {
	Activate bool
}

type OrdinaryEmailArgumentsV1 struct{ Email string }

type CandidateReceiptLookupArgumentsV1 struct {
	InstanceStateRoot string
	AttemptID         string
	JSON              bool
}

type OperatorStatusArgumentsV1 struct {
	OperationID string
	JSON        bool
}

type OperatorRecoverArgumentsV1 struct {
	OperationID string
	JSON        bool
}

type ProxyStatusArgumentsV1 struct{ JSON bool }

type ModelsListArgumentsV1 struct {
	Provider string
	JSON     bool
}

type ModelsOverlayArgumentsV1 struct {
	Provider  string
	ID        string
	CloneFrom string
}

type CodexCaptureArgumentsV1 struct {
	Input           string
	Output          string
	ContentEncoding string
	Metadata        string
}

type CodexValidationArgumentsV1 struct {
	ClientBuild      string
	ClientExecutable string
	StateDir         string
}

type CheckCachePolicyV1 struct {
	InputClass          string
	EffectiveTTLSeconds int
	InitialLookup       string
}

var candidateReceiptIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func ClassifyProxyCommand(argv []string) (OrdinaryCommandAuthorityV1, error) {
	if len(argv) > 0 && argv[0] == "agent" {
		return classifyAgentAuthority(argv)
	}
	if len(argv) > 0 && argv[0] == "refresh" {
		if helpRequested(argv[1:]) {
			return terminatingOrdinary("ordinary_help"), nil
		}
		if len(argv) != 1 {
			return terminatingOrdinary("ordinary_usage_error"), nil
		}
		return OrdinaryCommandAuthorityV1{Catalogue: "refresh", Row: "refresh", Deadline: CommandDeadlineV1{Total: 300 * time.Second, Forward: 270 * time.Second, Reserve: 30 * time.Second}}, nil
	}
	if len(argv) > 0 && argv[0] == "operation" {
		return classifyOperatorRecoveryAuthority(argv)
	}
	if len(argv) > 0 && argv[0] == "models" {
		return classifyModelsAuthority(argv)
	}
	if len(argv) > 1 && argv[0] == "codex" && (argv[1] == "validate" || argv[1] == "canary") {
		return classifyCodexAuxiliaryAuthority(argv)
	}
	if len(argv) > 0 && argv[0] == "proxy" {
		return classifyProxyReadAuthority(argv)
	}
	return classifyOrdinaryAuthority(argv)
}

func ImplicitEnsureAgentAuthority() OrdinaryCommandAuthorityV1 {
	return OrdinaryCommandAuthorityV1{Catalogue: "refresh", Row: "implicit_ensure_agent", Deadline: CommandDeadlineV1{Total: 10 * time.Second, Forward: 5 * time.Second, Reserve: 5 * time.Second}}
}

func terminatingOrdinary(row string) OrdinaryCommandAuthorityV1 {
	return OrdinaryCommandAuthorityV1{Catalogue: "ordinary", Row: row, Terminating: true}
}

func classifyAgentAuthority(argv []string) (OrdinaryCommandAuthorityV1, error) {
	if len(argv) >= 2 && (argv[1] == "--help" || argv[1] == "-h") {
		return terminatingOrdinary("ordinary_help"), nil
	}
	if len(argv) >= 2 && argv[1] == "help" {
		if len(argv) == 2 || (len(argv) == 3 && (argv[2] == "install" || argv[2] == "uninstall")) {
			return terminatingOrdinary("ordinary_help"), nil
		}
		return terminatingOrdinary("ordinary_usage_error"), nil
	}
	if len(argv) < 2 || (argv[1] != "install" && argv[1] != "uninstall") {
		return terminatingOrdinary("ordinary_usage_error"), nil
	}
	if helpRequested(argv[2:]) {
		return terminatingOrdinary("ordinary_help"), nil
	}
	row := "agent_" + argv[1]
	return OrdinaryCommandAuthorityV1{
		Catalogue: "refresh",
		Row:       row,
		Deadline:  CommandDeadlineV1{Total: 30 * time.Second, Forward: 15 * time.Second, Reserve: 15 * time.Second},
	}, nil
}

func classifyOperatorRecoveryAuthority(argv []string) (OrdinaryCommandAuthorityV1, error) {
	if helpRequested(argv[1:]) {
		return terminatingOrdinary("ordinary_help"), nil
	}
	if len(argv) < 2 || (argv[1] != "status" && argv[1] != "recover") {
		return terminatingOrdinary("ordinary_usage_error"), nil
	}
	operationID, jsonOutput, ok := parseOperationArguments(argv[2:])
	if !ok || (operationID != "" && !candidateReceiptIDPattern.MatchString(operationID)) || (argv[1] == "recover" && operationID == "") {
		return terminatingOrdinary("ordinary_usage_error"), nil
	}
	var arguments any = OperatorStatusArgumentsV1{OperationID: operationID, JSON: jsonOutput}
	if argv[1] == "recover" {
		arguments = OperatorRecoverArgumentsV1{OperationID: operationID, JSON: jsonOutput}
	}
	return OrdinaryCommandAuthorityV1{Catalogue: "operator_recovery", Row: "operator_" + argv[1], Arguments: arguments, Deadline: CommandDeadlineV1{Total: 30 * time.Second, Forward: 15 * time.Second, Reserve: 15 * time.Second}}, nil
}

func parseOperationArguments(argv []string) (string, bool, bool) {
	var operationID string
	var jsonOutput bool
	for index := 0; index < len(argv); index++ {
		switch argv[index] {
		case "--json":
			if jsonOutput {
				return "", false, false
			}
			jsonOutput = true
		case "--operation-id":
			if operationID != "" || index+1 >= len(argv) {
				return "", false, false
			}
			operationID = argv[index+1]
			index++
		default:
			return "", false, false
		}
	}
	return operationID, jsonOutput, true
}

func classifyProxyReadAuthority(argv []string) (OrdinaryCommandAuthorityV1, error) {
	if len(argv) >= 2 && argv[1] == "status" {
		if helpRequested(argv[2:]) {
			return terminatingOrdinary("ordinary_help"), nil
		}
		if len(argv) == 2 {
			return OrdinaryCommandAuthorityV1{Catalogue: "proxy", Row: "proxy_status_frozen", Deadline: CommandDeadlineV1{Total: 5 * time.Second, Forward: 5 * time.Second}}, nil
		}
		if len(argv) == 3 && argv[2] == "--json" {
			return OrdinaryCommandAuthorityV1{Catalogue: "proxy", Row: "proxy_status", Arguments: ProxyStatusArgumentsV1{JSON: true}, Deadline: CommandDeadlineV1{Total: 10 * time.Second, Forward: 10 * time.Second}}, nil
		}
		if len(argv) == 4 && argv[2] == "--port" {
			port, err := strconv.Atoi(argv[3])
			if err == nil && port > 0 && port <= 65535 {
				return OrdinaryCommandAuthorityV1{Catalogue: "proxy", Row: "proxy_status_frozen", Deadline: CommandDeadlineV1{Total: 5 * time.Second, Forward: 5 * time.Second}}, nil
			}
		}
		return terminatingOrdinary("ordinary_usage_error"), nil
	}
	if len(argv) < 4 || argv[1] != "candidate" || argv[2] != "receipt" || argv[3] != "show" {
		return terminatingOrdinary("ordinary_usage_error"), nil
	}
	if helpRequested(argv[4:]) {
		return terminatingOrdinary("ordinary_help"), nil
	}
	var arguments CandidateReceiptLookupArgumentsV1
	var seenRoot, seenAttempt, seenJSON bool
	for index := 4; index < len(argv); index++ {
		switch argv[index] {
		case "--instance-state-root":
			if seenRoot || index+1 >= len(argv) {
				return terminatingOrdinary("ordinary_usage_error"), nil
			}
			seenRoot = true
			arguments.InstanceStateRoot = argv[index+1]
			index++
		case "--attempt-id":
			if seenAttempt || index+1 >= len(argv) {
				return terminatingOrdinary("ordinary_usage_error"), nil
			}
			seenAttempt = true
			arguments.AttemptID = argv[index+1]
			index++
		case "--json":
			if seenJSON {
				return terminatingOrdinary("ordinary_usage_error"), nil
			}
			seenJSON = true
			arguments.JSON = true
		default:
			return terminatingOrdinary("ordinary_usage_error"), nil
		}
	}
	if arguments.InstanceStateRoot == "" || !candidateReceiptIDPattern.MatchString(arguments.AttemptID) {
		return terminatingOrdinary("ordinary_usage_error"), nil
	}
	return OrdinaryCommandAuthorityV1{Catalogue: "proxy", Row: "candidate_receipt_show", Arguments: arguments, Deadline: CommandDeadlineV1{Total: 10 * time.Second, Forward: 10 * time.Second}}, nil
}

func classifyModelsAuthority(argv []string) (OrdinaryCommandAuthorityV1, error) {
	if helpRequested(argv[1:]) {
		return terminatingOrdinary("ordinary_help"), nil
	}
	if len(argv) < 2 {
		return terminatingOrdinary("ordinary_usage_error"), nil
	}
	mutationDeadline := CommandDeadlineV1{Total: 60 * time.Second, Forward: 45 * time.Second, Reserve: 15 * time.Second}
	switch argv[1] {
	case "list":
		arguments, ok := parseModelsListArguments(argv[2:])
		if !ok {
			return terminatingOrdinary("ordinary_usage_error"), nil
		}
		return OrdinaryCommandAuthorityV1{Catalogue: "models", Row: "models_list", Arguments: arguments, Deadline: CommandDeadlineV1{Total: 10 * time.Second, Forward: 10 * time.Second}}, nil
	case "refresh":
		return OrdinaryCommandAuthorityV1{Catalogue: "models", Row: "models_refresh", Arguments: struct{}{}, Deadline: mutationDeadline}, nil
	case "overlay":
		if len(argv) < 3 {
			return terminatingOrdinary("ordinary_usage_error"), nil
		}
		switch argv[2] {
		case "add", "remove":
			arguments, ok := parseModelsOverlayArguments(argv[3:], argv[2] == "add")
			if !ok {
				return terminatingOrdinary("ordinary_usage_error"), nil
			}
			return OrdinaryCommandAuthorityV1{Catalogue: "models", Row: "models_overlay_" + argv[2], Arguments: arguments, Deadline: mutationDeadline}, nil
		case "prune":
			return OrdinaryCommandAuthorityV1{Catalogue: "models", Row: "models_overlay_prune", Arguments: struct{}{}, Deadline: mutationDeadline}, nil
		}
	}
	return terminatingOrdinary("ordinary_usage_error"), nil
}

func parseModelsListArguments(argv []string) (ModelsListArgumentsV1, bool) {
	var arguments ModelsListArgumentsV1
	for index := 0; index < len(argv); index++ {
		switch argv[index] {
		case "--json":
			if arguments.JSON {
				return ModelsListArgumentsV1{}, false
			}
			arguments.JSON = true
		case "--provider":
			if arguments.Provider != "" || index+1 >= len(argv) || (argv[index+1] != "anthropic" && argv[index+1] != "codex") {
				return ModelsListArgumentsV1{}, false
			}
			arguments.Provider = argv[index+1]
			index++
		default:
			return ModelsListArgumentsV1{}, false
		}
	}
	return arguments, true
}

func parseModelsOverlayArguments(argv []string, allowClone bool) (ModelsOverlayArgumentsV1, bool) {
	var arguments ModelsOverlayArgumentsV1
	for index := 0; index < len(argv); index++ {
		if index+1 >= len(argv) {
			return ModelsOverlayArgumentsV1{}, false
		}
		value := argv[index+1]
		switch argv[index] {
		case "--provider":
			if arguments.Provider != "" || (value != "anthropic" && value != "codex") {
				return ModelsOverlayArgumentsV1{}, false
			}
			arguments.Provider = value
		case "--id":
			if arguments.ID != "" || strings.TrimSpace(value) == "" {
				return ModelsOverlayArgumentsV1{}, false
			}
			arguments.ID = value
		case "--clone-from":
			if !allowClone || arguments.CloneFrom != "" || strings.TrimSpace(value) == "" {
				return ModelsOverlayArgumentsV1{}, false
			}
			arguments.CloneFrom = value
		default:
			return ModelsOverlayArgumentsV1{}, false
		}
		index++
	}
	return arguments, arguments.Provider != "" && arguments.ID != ""
}

func classifyCodexAuxiliaryAuthority(argv []string) (OrdinaryCommandAuthorityV1, error) {
	if helpRequested(argv[2:]) {
		return terminatingOrdinary("ordinary_help"), nil
	}
	if len(argv) < 3 {
		return terminatingOrdinary("ordinary_usage_error"), nil
	}
	if argv[1] == "canary" {
		if len(argv) != 3 {
			return terminatingOrdinary("ordinary_usage_error"), nil
		}
		deadlines := map[string]CommandDeadlineV1{
			"start":  {Total: 30 * time.Second, Forward: 15 * time.Second, Reserve: 15 * time.Second},
			"status": {Total: 10 * time.Second, Forward: 10 * time.Second},
			"stop":   {Total: 30 * time.Second, Forward: 15 * time.Second, Reserve: 15 * time.Second},
		}
		deadline, ok := deadlines[argv[2]]
		if !ok {
			return terminatingOrdinary("ordinary_usage_error"), nil
		}
		return OrdinaryCommandAuthorityV1{Catalogue: "codex_auxiliary", Row: "canary_" + argv[2], Arguments: struct{}{}, Deadline: deadline}, nil
	}
	switch argv[2] {
	case "capture":
		values, ok := parseExactFlagPairs(argv[3:], map[string]bool{"--input": true, "--output": true, "--content-encoding": true, "--metadata": true})
		if !ok || values["--input"] == "" || values["--output"] == "" {
			return terminatingOrdinary("ordinary_usage_error"), nil
		}
		arguments := CodexCaptureArgumentsV1{Input: values["--input"], Output: values["--output"], ContentEncoding: values["--content-encoding"], Metadata: values["--metadata"]}
		return OrdinaryCommandAuthorityV1{Catalogue: "codex_auxiliary", Row: "validate_capture", Arguments: arguments, Deadline: CommandDeadlineV1{Total: 30 * time.Second, Forward: 20 * time.Second, Reserve: 10 * time.Second}}, nil
	case "http", "websocket":
		allowed := map[string]bool{"--client-build": true, "--state-dir": true}
		if argv[2] == "websocket" {
			allowed["--client-executable"] = true
		}
		values, ok := parseExactFlagPairs(argv[3:], allowed)
		if !ok || strings.TrimSpace(values["--client-build"]) == "" {
			return terminatingOrdinary("ordinary_usage_error"), nil
		}
		arguments := CodexValidationArgumentsV1{ClientBuild: values["--client-build"], ClientExecutable: values["--client-executable"], StateDir: values["--state-dir"]}
		deadline := CommandDeadlineV1{Total: 10 * time.Second, Forward: 10 * time.Second}
		if argv[2] == "websocket" {
			deadline = CommandDeadlineV1{Total: 45 * time.Second, Forward: 30 * time.Second, Reserve: 15 * time.Second}
		}
		return OrdinaryCommandAuthorityV1{Catalogue: "codex_auxiliary", Row: "validate_" + argv[2], Arguments: arguments, Deadline: deadline}, nil
	}
	return terminatingOrdinary("ordinary_usage_error"), nil
}

func parseExactFlagPairs(argv []string, allowed map[string]bool) (map[string]string, bool) {
	values := make(map[string]string, len(argv)/2)
	seen := make(map[string]bool, len(argv)/2)
	if len(argv)%2 != 0 {
		return nil, false
	}
	for index := 0; index < len(argv); index += 2 {
		if !allowed[argv[index]] || seen[argv[index]] {
			return nil, false
		}
		seen[argv[index]] = true
		values[argv[index]] = argv[index+1]
	}
	return values, true
}

func classifyOrdinaryAuthority(argv []string) (OrdinaryCommandAuthorityV1, error) {
	clean, jsonOutput, refresh, terminal := stripOrdinaryGlobalFlags(argv)
	if terminal != "" {
		return terminatingOrdinary(terminal), nil
	}
	if clean == nil {
		return terminatingOrdinary("ordinary_usage_error"), nil
	}
	if len(clean) == 0 {
		return checkAuthority(nil, jsonOutput, refresh), nil
	}
	if clean[0] == "check" {
		providers := clean[1:]
		if !validProviders(providers) || globalFlagInsideProviderSlice(argv) {
			return terminatingOrdinary("ordinary_usage_error"), nil
		}
		return checkAuthority(providers, jsonOutput, refresh), nil
	}
	if clean[0] == "claude" || clean[0] == "codex" {
		if len(clean) < 2 {
			return terminatingOrdinary("ordinary_usage_error"), nil
		}
		provider, leaf := clean[0], clean[1]
		row := provider + "_" + leaf
		switch leaf {
		case "login":
			if len(clean) > 3 || (len(clean) == 3 && clean[2] != "--activate") {
				return terminatingOrdinary("ordinary_usage_error"), nil
			}
			return OrdinaryCommandAuthorityV1{Catalogue: "ordinary", Row: row, Arguments: OrdinaryLoginArgumentsV1{Activate: len(clean) == 3}, Deadline: deadlineForOrdinary(row)}, nil
		case "accounts":
			if len(clean) != 2 {
				return terminatingOrdinary("ordinary_usage_error"), nil
			}
			return OrdinaryCommandAuthorityV1{Catalogue: "ordinary", Row: row, Arguments: struct{}{}, Deadline: deadlineForOrdinary(row)}, nil
		case "switch", "remove":
			if len(clean) != 3 {
				return terminatingOrdinary("ordinary_usage_error"), nil
			}
			return OrdinaryCommandAuthorityV1{Catalogue: "ordinary", Row: row, Arguments: OrdinaryEmailArgumentsV1{Email: clean[2]}, Deadline: deadlineForOrdinary(row)}, nil
		}
	}
	if len(clean) == 2 && clean[0] == "gemini" && clean[1] == "accounts" {
		return OrdinaryCommandAuthorityV1{Catalogue: "ordinary", Row: "gemini_accounts", Arguments: struct{}{}, Deadline: deadlineForOrdinary("gemini_accounts")}, nil
	}
	return terminatingOrdinary("ordinary_usage_error"), nil
}

func checkAuthority(providers []string, jsonOutput, refresh bool) OrdinaryCommandAuthorityV1 {
	return OrdinaryCommandAuthorityV1{
		Catalogue: "ordinary",
		Row:       "check",
		Arguments: OrdinaryCheckArgumentsV1{Providers: append([]string(nil), providers...), JSON: jsonOutput, Refresh: refresh, Cache: ParseCheckCachePolicyV1("", refresh)},
		Deadline:  deadlineForOrdinary("check"),
	}
}

func deadlineForOrdinary(row string) CommandDeadlineV1 {
	if strings.HasSuffix(row, "_login") {
		return CommandDeadlineV1{Total: 360 * time.Second, Forward: 315 * time.Second, Reserve: 45 * time.Second}
	}
	if strings.HasSuffix(row, "_accounts") || row == "gemini_accounts" {
		return CommandDeadlineV1{Total: 30 * time.Second, Forward: 20 * time.Second, Reserve: 10 * time.Second}
	}
	return CommandDeadlineV1{Total: 120 * time.Second, Forward: 90 * time.Second, Reserve: 30 * time.Second}
}

func stripOrdinaryGlobalFlags(argv []string) ([]string, bool, bool, string) {
	clean := make([]string, 0, len(argv))
	var jsonOutput, refresh bool
	for index, argument := range argv {
		if argument == "--" {
			clean = append(clean, argv[index+1:]...)
			break
		}
		switch {
		case argument == "help" || argument == "--help" || argument == "-h":
			return clean, jsonOutput, refresh, "ordinary_help"
		case argument == "--version" || argument == "-v":
			return clean, jsonOutput, refresh, "ordinary_version"
		case argument == "--json" || argument == "-j":
			jsonOutput = true
		case argument == "--refresh" || argument == "-r":
			refresh = true
		case strings.HasPrefix(argument, "--json="):
			value, err := strconv.ParseBool(strings.ToLower(strings.TrimPrefix(argument, "--json=")))
			if err != nil {
				return nil, false, false, ""
			}
			jsonOutput = value
		case strings.HasPrefix(argument, "--refresh="):
			value, err := strconv.ParseBool(strings.ToLower(strings.TrimPrefix(argument, "--refresh=")))
			if err != nil {
				return nil, false, false, ""
			}
			refresh = value
		case strings.HasPrefix(argument, "-") && len(argument) > 2 && onlyJR(argument[1:]):
			for _, flag := range argument[1:] {
				if flag == 'j' {
					jsonOutput = true
				} else {
					refresh = true
				}
			}
		default:
			clean = append(clean, argument)
		}
	}
	return clean, jsonOutput, refresh, ""
}

func onlyJR(value string) bool {
	for _, flag := range value {
		if flag != 'j' && flag != 'r' {
			return false
		}
	}
	return value != ""
}

func globalFlagInsideProviderSlice(argv []string) bool {
	seenProvider := false
	seenFlagAfterProvider := false
	for _, argument := range argv {
		if argument == "--" {
			break
		}
		if argument == "check" {
			continue
		}
		if argument == "claude" || argument == "codex" || argument == "gemini" {
			if seenFlagAfterProvider {
				return true
			}
			seenProvider = true
			continue
		}
		if seenProvider && isOrdinaryGlobalFlag(argument) {
			seenFlagAfterProvider = true
		}
	}
	return false
}

func isOrdinaryGlobalFlag(argument string) bool {
	return argument == "--json" || argument == "-j" || argument == "--refresh" || argument == "-r" || strings.HasPrefix(argument, "--json=") || strings.HasPrefix(argument, "--refresh=") || (strings.HasPrefix(argument, "-") && onlyJR(argument[1:]))
}

func validProviders(providers []string) bool {
	for _, provider := range providers {
		if provider != "claude" && provider != "codex" && provider != "gemini" {
			return false
		}
	}
	return true
}

func ParseCheckCachePolicyV1(input string, refresh bool) CheckCachePolicyV1 {
	value, err := strconv.Atoi(input)
	inputClass := "in_range"
	if input == "" {
		value, inputClass = 30, "empty"
	} else if err != nil {
		value, inputClass = 30, "invalid"
	} else if value < 0 {
		value, inputClass = 0, "negative_clamped"
	} else if value > 3600 {
		value, inputClass = 3600, "above_max_clamped"
	}
	lookup := "serve_if_age_lte_ttl"
	if refresh {
		lookup = "bypass"
	}
	return CheckCachePolicyV1{InputClass: inputClass, EffectiveTTLSeconds: value, InitialLookup: lookup}
}
