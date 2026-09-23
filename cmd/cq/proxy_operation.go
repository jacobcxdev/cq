package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

var inspectOperatorOperation = defaultInspectOperatorOperation
var inspectCandidateReceipt = func(ctx context.Context, root, attemptID string) (proxy.CandidateReceiptInspectionV1, error) {
	return lookupCandidateReceipt(ctx, fsutil.OSFileSystem{}, root, attemptID)
}

type operatorOperationEnvelopeV1 struct {
	SchemaVersion int                       `json:"schema_version"`
	Kind          string                    `json:"kind"`
	OK            bool                      `json:"ok"`
	State         string                    `json:"state"`
	Result        operatorOperationResultV1 `json:"result"`
	Warnings      []ProxyWarningV1          `json:"warnings"`
	Errors        []ProxyErrorV1            `json:"errors"`
}

type operatorOperationResultV1 struct {
	OperationID      *string `json:"operation_id"`
	OperationAction  string  `json:"operation_action"`
	RecoveryControl  string  `json:"recovery_control"`
	Phase            *string `json:"phase"`
	ResultAvailable  bool    `json:"result_available"`
	SafeResultDigest *string `json:"safe_result_digest"`
	Remediation      *string `json:"remediation_command"`
}

func defaultInspectOperatorOperation(ctx context.Context, operationID, _ string) (proxy.OperationCoordinatorInspectionV1, error) {
	paths, err := proxy.ResolveDefaultPaths(userdirs.ConfigRoot)
	if err != nil {
		return proxy.OperationCoordinatorInspectionV1{}, err
	}
	return inspectOperatorOperationAt(ctx, operationID, paths)
}

func inspectOperatorOperationAt(ctx context.Context, operationID string, paths proxy.DefaultPaths) (proxy.OperationCoordinatorInspectionV1, error) {
	cfg, err := proxy.LoadExistingConfigAt(paths)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return proxy.OperationCoordinatorInspectionV1{}, nil
		}
		return proxy.OperationCoordinatorInspectionV1{}, err
	}
	if cfg.ProxyResilienceStateDir == "" {
		return proxy.OperationCoordinatorInspectionV1{}, nil
	}
	return proxy.InspectOperationCoordinatorState(ctx, fsutil.OSFileSystem{}, cfg.ProxyResilienceStateDir, operationID)
}

func runOperatorOperation(ctx context.Context, output io.Writer, action, operationID string, jsonOutput bool) (int, error) {
	inspection, inspectErr := inspectOperatorOperation(ctx, operationID, action)
	state, exitCode := "idle", 0
	result := operatorOperationResultV1{OperationAction: action, RecoveryControl: "not_required"}
	if operationID != "" {
		result.OperationID = &operationID
	}
	errorsOut := []ProxyErrorV1{}
	if inspectErr != nil {
		state, exitCode = "indeterminate", 4
		errorsOut = append(errorsOut, operationResultError("operator_operation_indeterminate", exitCode, "operation state is unavailable"))
	} else if !inspection.Found {
		if operationID != "" {
			state, exitCode = "failed", 3
			errorsOut = append(errorsOut, operationResultError("requested_object_absent", exitCode, "requested operation does not exist"))
		}
	} else {
		result.OperationID = &inspection.OperationID
		result.Phase = &inspection.Phase
		result.SafeResultDigest = &inspection.ValueDigest
		result.ResultAvailable = inspection.Receipt || inspection.Phase == "terminal"
		if result.ResultAvailable {
			state = "succeeded"
		} else if action == "recover" {
			state, exitCode = "indeterminate", 4
			result.RecoveryControl = "unavailable"
			errorsOut = append(errorsOut, operationResultError("operator_operation_indeterminate", exitCode, "operation recovery control is unavailable"))
		} else {
			state, exitCode = "pending", 1
		}
	}
	envelope := operatorOperationEnvelopeV1{SchemaVersion: 1, Kind: "operator_operation", OK: exitCode == 0, State: state, Result: result, Warnings: []ProxyWarningV1{}, Errors: errorsOut}
	if jsonOutput {
		return exitCode, json.NewEncoder(output).Encode(envelope)
	}
	if result.OperationID == nil {
		_, err := fmt.Fprintln(output, "operation: idle")
		return exitCode, err
	}
	_, err := fmt.Fprintf(output, "operation: %s\nstate: %s\n", *result.OperationID, state)
	return exitCode, err
}

func operationResultError(code string, exitCode int, message string) ProxyErrorV1 {
	return ProxyErrorV1{Code: code, Category: "operation", ExitCode: exitCode, Message: message, EvidenceRefs: []string{}}
}

type candidateReceiptEnvelopeV1 struct {
	SchemaVersion int                                `json:"schema_version"`
	Kind          string                             `json:"kind"`
	OK            bool                               `json:"ok"`
	State         string                             `json:"state"`
	Result        proxy.CandidateReceiptInspectionV1 `json:"result"`
	Warnings      []ProxyWarningV1                   `json:"warnings"`
	Errors        []ProxyErrorV1                     `json:"errors"`
}

func runCandidateReceiptLookup(ctx context.Context, output io.Writer, arguments CandidateReceiptLookupArgumentsV1) (int, error) {
	inspection, inspectErr := inspectCandidateReceipt(ctx, arguments.InstanceStateRoot, arguments.AttemptID)
	state, exitCode := "validated", 0
	errorsOut := []ProxyErrorV1{}
	if inspectErr != nil {
		state, exitCode = "failed", 4
		errorsOut = append(errorsOut, operationResultError("candidate_receipt_unavailable", exitCode, "candidate receipt is unavailable"))
	} else if !inspection.Found {
		state, exitCode = "failed", 3
		errorsOut = append(errorsOut, operationResultError("requested_object_absent", exitCode, "requested candidate receipt does not exist"))
	} else if inspection.Outcome == "conflicted" {
		state, exitCode = "conflicted", 3
		errorsOut = append(errorsOut, operationResultError("candidate_receipt_conflicted", exitCode, "candidate receipt is conflicted"))
	}
	envelope := candidateReceiptEnvelopeV1{SchemaVersion: 1, Kind: "proxy_validation_transition", OK: exitCode == 0, State: state, Result: inspection, Warnings: []ProxyWarningV1{}, Errors: errorsOut}
	if arguments.JSON {
		return exitCode, json.NewEncoder(output).Encode(envelope)
	}
	_, err := fmt.Fprintf(output, "candidate receipt: %s\nstate: %s\n", arguments.AttemptID, state)
	return exitCode, err
}

// lookupCandidateReceipt returns typed authenticated state, without rendering,
// locking, repairing, or selecting another attempt.
func lookupCandidateReceipt(ctx context.Context, fsys fsutil.FileSystem, root, attemptID string) (proxy.CandidateReceiptInspectionV1, error) {
	result, err := proxy.InspectCandidateReceiptState(ctx, fsys, root, attemptID)
	var pathErr *os.PathError
	if err != nil && ctx.Err() == nil && !errors.As(err, &pathErr) && !errors.Is(err, fsutil.ErrSecureCapabilityUnavailable) {
		return result, errors.Join(proxy.ErrCandidateLifecycleInvalid, err)
	}
	return result, err
}
