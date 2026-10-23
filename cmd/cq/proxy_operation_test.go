package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/jacobcxdev/cq/internal/proxy"
	"strings"
	"testing"
)

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
