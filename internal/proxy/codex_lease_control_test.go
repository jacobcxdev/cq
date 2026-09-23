package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
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

func TestCodexLeaseInvalidationErrorReceipts(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code string
	}{
		{"writer", ErrCodexLeaseWriterUnavailable, "lease_journal_unavailable"},
		{"poisoned", ErrCodexLeaseStorePoisoned, "lease_journal_unavailable"},
		{"legacy", ErrCodexLegacyQuarantine, "lease_journal_unavailable"},
		{"trust", ErrCodexLeaseTrustLost, "lease_journal_unavailable"},
		{"stale", ErrCodexLeaseStaleMutation, "routing_conflict"},
		{"authority", ErrCodexLeaseAuthorityMismatch, "routing_conflict"},
		{"invalid", ErrCodexLeaseInvalidMutation, "routing_conflict"},
		{"write", &fsutil.CommitError{Outcome: fsutil.CommitNotCommitted, Err: errors.New("synthetic failure")}, "routing_io_failed"},
		{"indeterminate poison", fmt.Errorf("%w: %w", ErrCodexLeaseStorePoisoned, &fsutil.CommitError{Outcome: fsutil.CommitIndeterminate, Err: errors.New("synthetic failure")}), "routing_io_failed"},
		{"unknown", errors.New("private diagnostic must not escape"), "routing_io_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{CodexLeaseInvalidator: &codexLeaseInvalidatorTestDouble{err: tc.err}}
			w := httptest.NewRecorder()
			s.handleCodexLeaseInvalidation(w, httptest.NewRequest(http.MethodPost, RuntimeCodexLeaseInvalidationPath, nil))
			if w.Code != 503 || w.Body.String() != "Codex lease invalidation unavailable\n" || w.Header().Get(CodexLeaseErrorHeader) != tc.code {
				t.Fatalf("status=%d body=%q receipt=%q", w.Code, w.Body.String(), w.Header().Get(CodexLeaseErrorHeader))
			}
		})
	}
}
func TestCodexLeaseInvalidationPriorPoisonHasNoCurrentCommitReceipt(t *testing.T) {
	coordinator, _, _ := openCodexLeaseRuntimeTestCoordinator(t)
	coordinator.store.poisoned = &fsutil.CommitError{Outcome: fsutil.CommitIndeterminate, Err: errors.New("earlier commit")}
	w := httptest.NewRecorder()
	(&Server{CodexLeaseInvalidator: coordinator}).handleCodexLeaseInvalidation(w, httptest.NewRequest(http.MethodPost, RuntimeCodexLeaseInvalidationPath, nil))
	if w.Code != 503 || w.Header().Get(CodexLeaseErrorHeader) != "lease_journal_unavailable" {
		t.Fatalf("prior poison status=%d receipt=%q", w.Code, w.Header().Get(CodexLeaseErrorHeader))
	}
}
