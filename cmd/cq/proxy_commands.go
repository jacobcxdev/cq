package main

import (
	"path/filepath"
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

type CandidatePrepareArgumentsV1 struct {
	InstanceStateRoot          string
	Port                       int
	SourceConfig               string
	TargetReleaseBundle        string
	TargetReleaseSet           string
	ClientBuild                string
	ClientExecutable           string
	LocalTokenClientRegistry   string
	CredentialMode             string
	CredentialManifest         string
	ConfirmReadOnlyCredentials bool
	PolicySnapshot             string
	ConfirmPayloadCapture      bool
	JSON                       bool
	Timeout                    time.Duration
}

type CandidateStatusArgumentsV1 struct {
	InstanceStateRoot string
	JSON              bool
}

type CandidateMutationArgumentsV1 struct {
	InstanceStateRoot    string
	JSON                 bool
	Timeout              time.Duration
	ConfirmClientStopped bool
}

type CandidateBarrierArgumentsV1 struct {
	InstanceStateRoot string
	ValidationRun     string
	JSON              bool
	Timeout           time.Duration
}

type CandidateArtifactSwitchArgumentsV1 struct {
	InstanceStateRoot     string
	Role                  string
	ReleaseSet            string
	ValidationRun         string
	ConfirmArtifactSwitch bool
	JSON                  bool
	Timeout               time.Duration
}

type CandidateValidateReleaseArgumentsV1 struct {
	InstanceStateRoot          string
	TargetReleaseBundle        string
	FloorReleaseBundle         string
	FloorAcceptanceReceiptFile string
	FloorAcceptanceReceipt     string
	ClientBuild                string
	ClientExecutable           string
	ValidationRun              string
	ReceiptOut                 string
	ConfirmLiveDataPlane       bool
	ConfirmQuotaUse            bool
	JSON                       bool
}

type CandidateRemoveArgumentsV1 struct {
	InstanceStateRoot         string
	ConfirmCandidateStateLoss bool
	JSON                      bool
	Timeout                   time.Duration
}

type OperatorStatusArgumentsV1 struct {
	OperationID string
	JSON        bool
}

type OperatorRecoverArgumentsV1 struct {
	OperationID string
	JSON        bool
}

type ProxyStatusArgumentsV1 struct {
	InstanceStateRoot string
	JSON              bool
	Human             bool
	Strict            bool
	Timeout           time.Duration
}

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
		if len(argv) == 4 && argv[2] == "--port" {
			port, err := strconv.Atoi(argv[3])
			if err == nil && port > 0 && port <= 65535 {
				return OrdinaryCommandAuthorityV1{Catalogue: "proxy", Row: "proxy_status_frozen", Deadline: CommandDeadlineV1{Total: 5 * time.Second, Forward: 5 * time.Second}}, nil
			}
		}
		arguments, ok := parseProxyStatusArguments(argv[2:])
		if ok {
			deadline := arguments.Timeout
			if deadline == 0 {
				deadline = 10 * time.Second
			}
			return OrdinaryCommandAuthorityV1{Catalogue: "proxy", Row: "proxy_status", Arguments: arguments, Deadline: CommandDeadlineV1{Total: deadline, Forward: deadline}}, nil
		}
		return terminatingOrdinary("ordinary_usage_error"), nil
	}
	if len(argv) >= 2 && argv[1] == "rescue" {
		if helpRequested(argv[2:]) {
			return terminatingOrdinary("ordinary_help"), nil
		}
		if !validProxyRescueArguments(argv[2:]) {
			return terminatingOrdinary("ordinary_usage_error"), nil
		}
		return OrdinaryCommandAuthorityV1{Catalogue: "proxy", Row: "proxy_rescue", Deadline: CommandDeadlineV1{Total: 10 * time.Second, Forward: 10 * time.Second}}, nil
	}
	if len(argv) >= 3 && argv[1] == "policy" && argv[2] == "status" {
		if helpRequested(argv[3:]) {
			return terminatingOrdinary("ordinary_help"), nil
		}
		if _, err := parseProxyPolicyOptions(argv[3:]); err != nil {
			return terminatingOrdinary("ordinary_usage_error"), nil
		}
		return OrdinaryCommandAuthorityV1{Catalogue: "proxy", Row: "proxy_policy_status", Deadline: CommandDeadlineV1{Total: 10 * time.Second, Forward: 10 * time.Second}}, nil
	}
	if len(argv) >= 2 && argv[1] == "candidate" {
		return classifyCandidateAuthority(argv)
	}
	return terminatingOrdinary("ordinary_usage_error"), nil
}

func classifyCandidateAuthority(argv []string) (OrdinaryCommandAuthorityV1, error) {
	if len(argv) < 3 {
		return terminatingOrdinary("ordinary_usage_error"), nil
	}
	if helpRequested(argv[2:]) {
		return terminatingOrdinary("ordinary_help"), nil
	}
	switch argv[2] {
	case "prepare":
		arguments, ok := parseCandidatePrepareArguments(argv[3:])
		if !ok {
			return terminatingOrdinary("ordinary_usage_error"), nil
		}
		return OrdinaryCommandAuthorityV1{Catalogue: "proxy", Row: "candidate_prepare", Arguments: arguments, Deadline: CommandDeadlineV1{Total: arguments.Timeout, Forward: arguments.Timeout - 30*time.Second, Reserve: 30 * time.Second}}, nil
	case "status":
		arguments, ok := parseCandidateStatusArguments(argv[3:])
		if !ok {
			return terminatingOrdinary("ordinary_usage_error"), nil
		}
		return OrdinaryCommandAuthorityV1{Catalogue: "proxy", Row: "candidate_status", Arguments: arguments, Deadline: CommandDeadlineV1{Total: 10 * time.Second, Forward: 10 * time.Second}}, nil
	case "start", "stop":
		arguments, ok := parseCandidateMutationArguments(argv[2], argv[3:])
		if !ok {
			return terminatingOrdinary("ordinary_usage_error"), nil
		}
		return OrdinaryCommandAuthorityV1{Catalogue: "proxy", Row: "candidate_" + argv[2], Arguments: arguments, Deadline: CommandDeadlineV1{Total: arguments.Timeout, Forward: arguments.Timeout - 15*time.Second, Reserve: 15 * time.Second}}, nil
	case "client-bearer-barrier":
		arguments, ok := parseCandidateBarrierArguments(argv[3:])
		if !ok {
			return terminatingOrdinary("ordinary_usage_error"), nil
		}
		return OrdinaryCommandAuthorityV1{Catalogue: "proxy", Row: "candidate_barrier_refresh", Arguments: arguments, Deadline: CommandDeadlineV1{Total: arguments.Timeout, Forward: arguments.Timeout - 30*time.Second, Reserve: 30 * time.Second}}, nil
	case "artifact":
		arguments, ok := parseCandidateArtifactSwitchArguments(argv[3:])
		if !ok {
			return terminatingOrdinary("ordinary_usage_error"), nil
		}
		return OrdinaryCommandAuthorityV1{Catalogue: "proxy", Row: "candidate_artifact_switch", Arguments: arguments, Deadline: CommandDeadlineV1{Total: arguments.Timeout, Forward: arguments.Timeout - 30*time.Second, Reserve: 30 * time.Second}}, nil
	case "validate-release":
		arguments, ok := parseCandidateValidateReleaseArguments(argv[3:])
		if !ok {
			return terminatingOrdinary("ordinary_usage_error"), nil
		}
		return OrdinaryCommandAuthorityV1{Catalogue: "proxy", Row: "candidate_validate_release", Arguments: arguments, Deadline: CommandDeadlineV1{Total: 16 * time.Minute, Forward: 15*time.Minute + 30*time.Second, Reserve: 30 * time.Second}}, nil
	case "remove":
		arguments, ok := parseCandidateRemoveArguments(argv[3:])
		if !ok {
			return terminatingOrdinary("ordinary_usage_error"), nil
		}
		return OrdinaryCommandAuthorityV1{Catalogue: "proxy", Row: "candidate_remove", Arguments: arguments, Deadline: CommandDeadlineV1{Total: arguments.Timeout, Forward: arguments.Timeout - 15*time.Second, Reserve: 15 * time.Second}}, nil
	case "receipt":
		return classifyCandidateReceiptAuthority(argv)
	default:
		return terminatingOrdinary("ordinary_usage_error"), nil
	}
}

func classifyCandidateReceiptAuthority(argv []string) (OrdinaryCommandAuthorityV1, error) {
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

func parseCandidatePrepareArguments(argv []string) (CandidatePrepareArgumentsV1, bool) {
	var arguments CandidatePrepareArgumentsV1
	seen := map[string]bool{}
	for index := 0; index < len(argv); index++ {
		arg := argv[index]
		switch arg {
		case "--json", "--confirm-read-only-credentials", "--confirm-payload-capture":
			if seen[arg] {
				return CandidatePrepareArgumentsV1{}, false
			}
			seen[arg] = true
			switch arg {
			case "--json":
				arguments.JSON = true
			case "--confirm-read-only-credentials":
				arguments.ConfirmReadOnlyCredentials = true
			case "--confirm-payload-capture":
				arguments.ConfirmPayloadCapture = true
			}
		case "--instance-state-root", "--port", "--source-config", "--target-release-bundle", "--target-release-set", "--client-build", "--client-executable", "--local-token-client-registry", "--credential-mode", "--credential-manifest", "--policy-snapshot":
			if seen[arg] || index+1 >= len(argv) {
				return CandidatePrepareArgumentsV1{}, false
			}
			seen[arg] = true
			value := argv[index+1]
			index++
			switch arg {
			case "--instance-state-root":
				arguments.InstanceStateRoot = value
			case "--port":
				port, err := strconv.Atoi(value)
				if err != nil {
					return CandidatePrepareArgumentsV1{}, false
				}
				arguments.Port = port
			case "--source-config":
				arguments.SourceConfig = value
			case "--target-release-bundle":
				arguments.TargetReleaseBundle = value
			case "--target-release-set":
				arguments.TargetReleaseSet = value
			case "--client-build":
				arguments.ClientBuild = value
			case "--client-executable":
				arguments.ClientExecutable = value
			case "--local-token-client-registry":
				arguments.LocalTokenClientRegistry = value
			case "--credential-mode":
				arguments.CredentialMode = value
			case "--credential-manifest":
				arguments.CredentialManifest = value
			case "--policy-snapshot":
				arguments.PolicySnapshot = value
			}
		default:
			if arguments.Timeout != 0 || index != len(argv)-1 {
				return CandidatePrepareArgumentsV1{}, false
			}
			duration, err := time.ParseDuration(arg)
			if err != nil || duration < 150*time.Second || duration > 5*time.Minute {
				return CandidatePrepareArgumentsV1{}, false
			}
			arguments.Timeout = duration
		}
	}
	if !cleanAbsolutePath(arguments.InstanceStateRoot) || arguments.Port < 1 || arguments.Port > 65535 || arguments.Port == 19280 || arguments.SourceConfig == "" || arguments.TargetReleaseBundle == "" || !lowerHexArgument(arguments.TargetReleaseSet, 64) || arguments.ClientBuild == "" || arguments.ClientExecutable == "" || arguments.LocalTokenClientRegistry == "" || arguments.Timeout == 0 {
		return CandidatePrepareArgumentsV1{}, false
	}
	switch arguments.CredentialMode {
	case "none":
		if arguments.CredentialManifest != "" || arguments.ConfirmReadOnlyCredentials {
			return CandidatePrepareArgumentsV1{}, false
		}
	case "read-only":
		if arguments.CredentialManifest == "" || !arguments.ConfirmReadOnlyCredentials {
			return CandidatePrepareArgumentsV1{}, false
		}
	default:
		return CandidatePrepareArgumentsV1{}, false
	}
	return arguments, true
}

func parseCandidateStatusArguments(argv []string) (CandidateStatusArgumentsV1, bool) {
	var arguments CandidateStatusArgumentsV1
	for index := 0; index < len(argv); index++ {
		switch argv[index] {
		case "--instance-state-root":
			if arguments.InstanceStateRoot != "" || index+1 >= len(argv) {
				return CandidateStatusArgumentsV1{}, false
			}
			arguments.InstanceStateRoot = argv[index+1]
			index++
		case "--json":
			if arguments.JSON {
				return CandidateStatusArgumentsV1{}, false
			}
			arguments.JSON = true
		default:
			return CandidateStatusArgumentsV1{}, false
		}
	}
	return arguments, cleanAbsolutePath(arguments.InstanceStateRoot)
}

func parseCandidateMutationArguments(action string, argv []string) (CandidateMutationArgumentsV1, bool) {
	var arguments CandidateMutationArgumentsV1
	for index := 0; index < len(argv); index++ {
		switch argv[index] {
		case "--instance-state-root":
			if arguments.InstanceStateRoot != "" || index+1 >= len(argv) {
				return CandidateMutationArgumentsV1{}, false
			}
			arguments.InstanceStateRoot = argv[index+1]
			index++
		case "--json":
			if arguments.JSON {
				return CandidateMutationArgumentsV1{}, false
			}
			arguments.JSON = true
		case "--confirm-client-stopped":
			if arguments.ConfirmClientStopped {
				return CandidateMutationArgumentsV1{}, false
			}
			arguments.ConfirmClientStopped = true
		default:
			if arguments.Timeout != 0 || index != len(argv)-1 {
				return CandidateMutationArgumentsV1{}, false
			}
			duration, err := time.ParseDuration(argv[index])
			maximum := 90 * time.Second
			if action == "stop" {
				maximum = 30 * time.Second
			}
			if err != nil || duration < 30*time.Second || duration > maximum {
				return CandidateMutationArgumentsV1{}, false
			}
			arguments.Timeout = duration
		}
	}
	if !cleanAbsolutePath(arguments.InstanceStateRoot) || arguments.Timeout == 0 || (action == "stop" && !arguments.ConfirmClientStopped) || (action == "start" && arguments.ConfirmClientStopped) {
		return CandidateMutationArgumentsV1{}, false
	}
	return arguments, true
}

func parseCandidateBarrierArguments(argv []string) (CandidateBarrierArgumentsV1, bool) {
	if len(argv) == 0 || argv[0] != "refresh" {
		return CandidateBarrierArgumentsV1{}, false
	}
	var arguments CandidateBarrierArgumentsV1
	for index := 1; index < len(argv); index++ {
		switch argv[index] {
		case "--instance-state-root":
			if arguments.InstanceStateRoot != "" || index+1 >= len(argv) {
				return CandidateBarrierArgumentsV1{}, false
			}
			arguments.InstanceStateRoot = argv[index+1]
			index++
		case "--validation-run":
			if arguments.ValidationRun != "" || index+1 >= len(argv) {
				return CandidateBarrierArgumentsV1{}, false
			}
			arguments.ValidationRun = argv[index+1]
			index++
		case "--json":
			if arguments.JSON {
				return CandidateBarrierArgumentsV1{}, false
			}
			arguments.JSON = true
		default:
			if arguments.Timeout != 0 || index != len(argv)-1 {
				return CandidateBarrierArgumentsV1{}, false
			}
			duration, err := time.ParseDuration(argv[index])
			if err != nil || duration < 150*time.Second || duration > 5*time.Minute {
				return CandidateBarrierArgumentsV1{}, false
			}
			arguments.Timeout = duration
		}
	}
	return arguments, cleanAbsolutePath(arguments.InstanceStateRoot) && lowerHexArgument(arguments.ValidationRun, 64) && arguments.Timeout > 0
}

func parseCandidateArtifactSwitchArguments(argv []string) (CandidateArtifactSwitchArgumentsV1, bool) {
	if len(argv) == 0 || argv[0] != "switch" {
		return CandidateArtifactSwitchArgumentsV1{}, false
	}
	var arguments CandidateArtifactSwitchArgumentsV1
	for index := 1; index < len(argv); index++ {
		switch argv[index] {
		case "--instance-state-root", "--role", "--release-set", "--validation-run":
			if index+1 >= len(argv) {
				return CandidateArtifactSwitchArgumentsV1{}, false
			}
			value := argv[index+1]
			index++
			switch argv[index-1] {
			case "--instance-state-root":
				if arguments.InstanceStateRoot != "" {
					return CandidateArtifactSwitchArgumentsV1{}, false
				}
				arguments.InstanceStateRoot = value
			case "--role":
				if arguments.Role != "" {
					return CandidateArtifactSwitchArgumentsV1{}, false
				}
				arguments.Role = value
			case "--release-set":
				if arguments.ReleaseSet != "" {
					return CandidateArtifactSwitchArgumentsV1{}, false
				}
				arguments.ReleaseSet = value
			case "--validation-run":
				if arguments.ValidationRun != "" {
					return CandidateArtifactSwitchArgumentsV1{}, false
				}
				arguments.ValidationRun = value
			}
		case "--confirm-artifact-switch":
			if arguments.ConfirmArtifactSwitch {
				return CandidateArtifactSwitchArgumentsV1{}, false
			}
			arguments.ConfirmArtifactSwitch = true
		case "--json":
			if arguments.JSON {
				return CandidateArtifactSwitchArgumentsV1{}, false
			}
			arguments.JSON = true
		default:
			if arguments.Timeout != 0 || index != len(argv)-1 {
				return CandidateArtifactSwitchArgumentsV1{}, false
			}
			duration, err := time.ParseDuration(argv[index])
			if err != nil || duration < 90*time.Second || duration > 2*time.Minute {
				return CandidateArtifactSwitchArgumentsV1{}, false
			}
			arguments.Timeout = duration
		}
	}
	return arguments, cleanAbsolutePath(arguments.InstanceStateRoot) && arguments.Role == "runtime-bundle" && lowerHexArgument(arguments.ReleaseSet, 64) && lowerHexArgument(arguments.ValidationRun, 64) && arguments.ConfirmArtifactSwitch && arguments.Timeout > 0
}

func parseCandidateValidateReleaseArguments(argv []string) (CandidateValidateReleaseArgumentsV1, bool) {
	var arguments CandidateValidateReleaseArgumentsV1
	seen := map[string]bool{}
	for index := 0; index < len(argv); index++ {
		arg := argv[index]
		switch arg {
		case "--confirm-live-data-plane", "--confirm-quota-use", "--json":
			if seen[arg] {
				return CandidateValidateReleaseArgumentsV1{}, false
			}
			seen[arg] = true
			switch arg {
			case "--confirm-live-data-plane":
				arguments.ConfirmLiveDataPlane = true
			case "--confirm-quota-use":
				arguments.ConfirmQuotaUse = true
			case "--json":
				arguments.JSON = true
			}
		case "--instance-state-root", "--target-release-bundle", "--floor-release-bundle", "--floor-acceptance-receipt-file", "--floor-acceptance-receipt", "--client-build", "--client-executable", "--validation-run", "--receipt-out":
			if seen[arg] || index+1 >= len(argv) {
				return CandidateValidateReleaseArgumentsV1{}, false
			}
			seen[arg] = true
			value := argv[index+1]
			index++
			switch arg {
			case "--instance-state-root":
				arguments.InstanceStateRoot = value
			case "--target-release-bundle":
				arguments.TargetReleaseBundle = value
			case "--floor-release-bundle":
				arguments.FloorReleaseBundle = value
			case "--floor-acceptance-receipt-file":
				arguments.FloorAcceptanceReceiptFile = value
			case "--floor-acceptance-receipt":
				arguments.FloorAcceptanceReceipt = value
			case "--client-build":
				arguments.ClientBuild = value
			case "--client-executable":
				arguments.ClientExecutable = value
			case "--validation-run":
				arguments.ValidationRun = value
			case "--receipt-out":
				arguments.ReceiptOut = value
			}
		default:
			return CandidateValidateReleaseArgumentsV1{}, false
		}
	}
	return arguments, cleanAbsolutePath(arguments.InstanceStateRoot) && arguments.TargetReleaseBundle != "" && arguments.FloorReleaseBundle != "" && arguments.FloorAcceptanceReceiptFile != "" && lowerHexArgument(arguments.FloorAcceptanceReceipt, 64) && arguments.ClientBuild != "" && arguments.ClientExecutable != "" && lowerHexArgument(arguments.ValidationRun, 64) && cleanAbsolutePath(arguments.ReceiptOut) && arguments.ConfirmLiveDataPlane && arguments.ConfirmQuotaUse
}

func parseCandidateRemoveArguments(argv []string) (CandidateRemoveArgumentsV1, bool) {
	var arguments CandidateRemoveArgumentsV1
	for index := 0; index < len(argv); index++ {
		switch argv[index] {
		case "--instance-state-root":
			if arguments.InstanceStateRoot != "" || index+1 >= len(argv) {
				return CandidateRemoveArgumentsV1{}, false
			}
			arguments.InstanceStateRoot = argv[index+1]
			index++
		case "--confirm-candidate-state-loss":
			if arguments.ConfirmCandidateStateLoss {
				return CandidateRemoveArgumentsV1{}, false
			}
			arguments.ConfirmCandidateStateLoss = true
		case "--json":
			if arguments.JSON {
				return CandidateRemoveArgumentsV1{}, false
			}
			arguments.JSON = true
		default:
			if arguments.Timeout != 0 || index != len(argv)-1 {
				return CandidateRemoveArgumentsV1{}, false
			}
			duration, err := time.ParseDuration(argv[index])
			if err != nil || duration != 30*time.Second {
				return CandidateRemoveArgumentsV1{}, false
			}
			arguments.Timeout = duration
		}
	}
	return arguments, cleanAbsolutePath(arguments.InstanceStateRoot) && arguments.ConfirmCandidateStateLoss && arguments.Timeout > 0
}

func cleanAbsolutePath(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path && path != string(filepath.Separator)
}

func lowerHexArgument(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}

func parseProxyStatusArguments(argv []string) (ProxyStatusArgumentsV1, bool) {
	var arguments ProxyStatusArgumentsV1
	for index := 0; index < len(argv); index++ {
		switch argv[index] {
		case "--json":
			if arguments.JSON || arguments.Human {
				return ProxyStatusArgumentsV1{}, false
			}
			arguments.JSON = true
		case "--human":
			if arguments.Human || arguments.JSON {
				return ProxyStatusArgumentsV1{}, false
			}
			arguments.Human = true
		case "--strict":
			if arguments.Strict {
				return ProxyStatusArgumentsV1{}, false
			}
			arguments.Strict = true
		case "--instance-state-root":
			if arguments.InstanceStateRoot != "" || index+1 >= len(argv) {
				return ProxyStatusArgumentsV1{}, false
			}
			root := argv[index+1]
			clean := filepath.Clean(root)
			if !filepath.IsAbs(root) || clean != root || clean == string(filepath.Separator) {
				return ProxyStatusArgumentsV1{}, false
			}
			arguments.InstanceStateRoot = root
			index++
		case "--timeout":
			if arguments.Timeout != 0 || index+1 >= len(argv) {
				return ProxyStatusArgumentsV1{}, false
			}
			duration, err := time.ParseDuration(argv[index+1])
			if err != nil || duration <= 0 {
				return ProxyStatusArgumentsV1{}, false
			}
			arguments.Timeout = duration
			index++
		default:
			if arguments.Timeout != 0 || index != len(argv)-1 {
				return ProxyStatusArgumentsV1{}, false
			}
			duration, err := time.ParseDuration(argv[index])
			if err != nil || duration <= 0 {
				return ProxyStatusArgumentsV1{}, false
			}
			arguments.Timeout = duration
		}
	}
	if !arguments.JSON && !arguments.Human && !arguments.Strict && arguments.Timeout == 0 && arguments.InstanceStateRoot == "" {
		return ProxyStatusArgumentsV1{}, false
	}
	return arguments, true
}

func validProxyRescueArguments(argv []string) bool {
	if len(argv) != 1 && len(argv) != 3 {
		return false
	}
	if argv[0] != "enter" && argv[0] != "exit" && argv[0] != "status" {
		return false
	}
	if len(argv) == 1 {
		return true
	}
	if argv[1] != "--port" {
		return false
	}
	port, err := strconv.Atoi(argv[2])
	return err == nil && port > 0 && port <= 65535
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
