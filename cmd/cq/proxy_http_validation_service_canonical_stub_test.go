//go:build !darwin

package main

import (
	"context"
	"errors"
	"testing"
)

func TestProxyHTTPValidationCanonicalUnsupportedBeforeStore(t *testing.T) {
	ops := canonicalHTTPValidationOperations{validate: validateCanonicalHTTPValidationCandidate, store: func(context.Context) (installedHTTPValidationRequestStore, error) {
		t.Fatal("unsupported platform accessed store")
		return installedHTTPValidationRequestStore{}, nil
	}}
	if err := runCanonicalProxyValidateHTTPWithOperations(context.Background(), 19281, "test", ops); !errors.Is(err, errValidationCandidateUnavailable) {
		t.Fatalf("err=%v", err)
	}
	exit, stdout, _ := runValidationCLI(t, []string{"codex", "proxy", "validate", "http", "--port", "19281", "--json"}, context.Background(), lookupV2Validation)
	if exit != 4 {
		t.Fatalf("exit=%d stdout=%s", exit, stdout)
	}
}
