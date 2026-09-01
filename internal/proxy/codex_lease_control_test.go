package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type codexLeaseInvalidatorTestDouble struct {
	result CodexLeaseInvalidationResult
	err    error
	calls  int
}

func (invalidator *codexLeaseInvalidatorTestDouble) InvalidateTaskAffinities(context.Context) (CodexLeaseInvalidationResult, error) {
	invalidator.calls++
	return invalidator.result, invalidator.err
}

func TestCodexLeaseControlInvalidatesWorkerOwnedAffinities(t *testing.T) {
	invalidator := &codexLeaseInvalidatorTestDouble{result: CodexLeaseInvalidationResult{InvalidatedLeases: 3, JournalGeneration: 42}}
	handler, err := (&Server{
		Config:                &Config{ClaudeUpstream: "https://example.test"},
		CodexLeaseInvalidator: invalidator,
	}).handler()
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, RuntimeCodexLeaseInvalidationPath, nil))
	if response.Code != http.StatusOK || invalidator.calls != 1 {
		t.Fatalf("response = %d %q, calls = %d", response.Code, response.Body.String(), invalidator.calls)
	}
	var got CodexLeaseInvalidationResult
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil || got != invalidator.result {
		t.Fatalf("result = %#v, error = %v", got, err)
	}
}

func TestCodexLeaseControlFailsClosedWhenAuthorityUnavailable(t *testing.T) {
	for _, test := range []struct {
		name        string
		invalidator CodexLeaseInvalidator
	}{
		{name: "missing"},
		{name: "failed", invalidator: &codexLeaseInvalidatorTestDouble{err: errors.New("failed")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, err := (&Server{
				Config:                &Config{ClaudeUpstream: "https://example.test"},
				CodexLeaseInvalidator: test.invalidator,
			}).handler()
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, RuntimeCodexLeaseInvalidationPath, nil))
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("response = %d %q", response.Code, response.Body.String())
			}
		})
	}
}

func TestCodexLeaseControlRequiresLocalCallerAuthority(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, RuntimeCodexLeaseInvalidationPath, nil)
	if got := normalCallerPolicy(request); got != normalCallerRouteLocal {
		t.Fatalf("normal caller policy = %d, want local", got)
	}
}
