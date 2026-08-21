package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestPureInspectionDispatchesOperationStatusAndRecover(t *testing.T) {
	oldInspect := inspectOperatorOperation
	t.Cleanup(func() { inspectOperatorOperation = oldInspect })
	var actions []string
	inspectOperatorOperation = func(_ context.Context, operationID, action string) (proxy.OperationCoordinatorInspectionV1, error) {
		actions = append(actions, action+":"+operationID)
		return proxy.OperationCoordinatorInspectionV1{Found: true, OperationID: operationID, Phase: "terminal", ValueDigest: strings.Repeat("a", 64), Receipt: true}, nil
	}

	operationID := strings.Repeat("1", 32)
	for _, args := range [][]string{
		{"operation", "status", "--operation-id", operationID, "--json"},
		{"operation", "recover", "--operation-id", operationID, "--json"},
	} {
		var output bytes.Buffer
		handled, exitCode, err := runPureGlobalInspection(args, &output, &bytes.Buffer{})
		if err != nil || !handled || exitCode != 0 {
			t.Fatalf("runPureGlobalInspection(%v) = %t, %d, %v", args, handled, exitCode, err)
		}
		if !strings.Contains(output.String(), `"kind":"operator_operation"`) || !strings.Contains(output.String(), operationID) {
			t.Fatalf("output = %q", output.String())
		}
	}
	if got, want := strings.Join(actions, ","), "status:"+operationID+",recover:"+operationID; got != want {
		t.Fatalf("actions = %q, want %q", got, want)
	}
}

func TestOperationRecoverDoesNotRecommendUnsupportedServiceCommand(t *testing.T) {
	oldInspect := inspectOperatorOperation
	t.Cleanup(func() { inspectOperatorOperation = oldInspect })
	inspectOperatorOperation = func(_ context.Context, operationID, _ string) (proxy.OperationCoordinatorInspectionV1, error) {
		return proxy.OperationCoordinatorInspectionV1{Found: true, OperationID: operationID, Phase: "executing", ValueDigest: strings.Repeat("a", 64)}, nil
	}
	var output bytes.Buffer
	exitCode, err := runOperatorOperation(context.Background(), &output, "recover", strings.Repeat("1", 32), true)
	if err != nil || exitCode != 4 {
		t.Fatalf("recover = exit %d err %v", exitCode, err)
	}
	var envelope operatorOperationEnvelopeV1
	if err := json.Unmarshal(output.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Result.Remediation != nil {
		t.Fatalf("unsupported remediation = %q", *envelope.Result.Remediation)
	}
}

func TestPureInspectionDispatchesCandidateReceiptLookup(t *testing.T) {
	oldInspect := inspectCandidateReceipt
	t.Cleanup(func() { inspectCandidateReceipt = oldInspect })
	attemptID := strings.Repeat("2", 32)
	inspectCandidateReceipt = func(_ context.Context, root, attempt string) (proxy.CandidateReceiptInspectionV1, error) {
		if root != "/tmp/candidate" || attempt != attemptID {
			t.Fatalf("lookup = %q %q", root, attempt)
		}
		return proxy.CandidateReceiptInspectionV1{Found: true, AttemptID: attempt, Outcome: "published", ReceiptDigest: strings.Repeat("b", 64)}, nil
	}

	var output bytes.Buffer
	handled, exitCode, err := runPureGlobalInspection([]string{"proxy", "candidate", "receipt", "show", "--instance-state-root", "/tmp/candidate", "--attempt-id", attemptID, "--json"}, &output, &bytes.Buffer{})
	if err != nil || !handled || exitCode != 0 {
		t.Fatalf("runPureGlobalInspection = %t, %d, %v", handled, exitCode, err)
	}
	if !strings.Contains(output.String(), `"kind":"proxy_validation_transition"`) || !strings.Contains(output.String(), attemptID) {
		t.Fatalf("output = %q", output.String())
	}
}

func TestOperationAndCandidateReceiptHelpStayPreState(t *testing.T) {
	for _, args := range [][]string{
		{"operation", "--help"},
		{"operation", "status", "--help"},
		{"operation", "recover", "--help"},
		{"proxy", "candidate", "receipt", "show", "--help"},
	} {
		var output bytes.Buffer
		handled, exitCode, err := runPureGlobalInspection(args, &output, &bytes.Buffer{})
		if err != nil || !handled || exitCode != 0 || !strings.Contains(output.String(), "Usage: cq") {
			t.Fatalf("runPureGlobalInspection(%v) = %t, %d, %v, %q", args, handled, exitCode, err, output.String())
		}
	}
}
