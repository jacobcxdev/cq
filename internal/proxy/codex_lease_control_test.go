package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/provider/codex"
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

type codexLeaseRedistributorTestDouble struct {
	calls      int
	resetCalls int
	account    codex.AccountKey
	event      string
	err        error
}

func (r *codexLeaseRedistributorTestDouble) RedistributeTaskAffinities(context.Context) (CodexLeaseRedistributionResult, error) {
	r.calls++
	return CodexLeaseRedistributionResult{ScheduledLeases: 3, DeferredLeases: 2, JournalGeneration: 42}, r.err
}
func (r *codexLeaseRedistributorTestDouble) RedistributeTaskAffinitiesForReset(_ context.Context, account codex.AccountKey, event string) (CodexLeaseRedistributionResult, error) {
	r.resetCalls++
	r.account, r.event = account, event
	return CodexLeaseRedistributionResult{ScheduledLeases: 3, DeferredLeases: 2, JournalGeneration: 42}, r.err
}

func TestCodexLeaseControlRedistributesWorkerOwnedChats(t *testing.T) {
	r := &codexLeaseRedistributorTestDouble{}
	handler, err := (&Server{Config: &Config{ClaudeUpstream: "https://example.test"}, CodexLeaseRedistributor: r}).handler()
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, RuntimeCodexLeaseRedistributionPath, nil)
	if normalCallerPolicy(request) != normalCallerRouteLocal {
		t.Fatal("redistribution lacked local caller policy")
	}
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || r.calls != 1 || r.resetCalls != 0 {
		t.Fatalf("response=%d, calls=%d/%d", response.Code, r.calls, r.resetCalls)
	}
	var result CodexLeaseRedistributionResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.ScheduledLeases != 3 || result.DeferredLeases != 2 || result.JournalGeneration != 42 {
		t.Fatalf("result=%#v, error=%v", result, err)
	}
}

func TestCodexLeaseControlResetRefreshesBeforeRedistribution(t *testing.T) {
	for _, fails := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "refresh_failed"}[fails], func(t *testing.T) {
			r := &codexLeaseRedistributorTestDouble{}
			refreshed := false
			s := &Server{CodexLeaseRedistributor: r, CodexResetCapacityRefresh: func(_ context.Context, account codex.AccountKey) error {
				if r.resetCalls != 0 || account != "account:test" {
					t.Fatal("refresh not before scheduling")
				}
				refreshed = true
				if fails {
					return errors.New("unavailable")
				}
				return nil
			}}
			response := httptest.NewRecorder()
			s.handleCodexLeaseRedistribution(response, httptest.NewRequest(http.MethodPost, RuntimeCodexLeaseRedistributionPath, strings.NewReader(`{"reset_account_key":"account:test","event_id":"event"}`)))
			if !refreshed || r.calls != 0 {
				t.Fatal("incorrect reset path")
			}
			if fails {
				if response.Code != http.StatusServiceUnavailable || r.resetCalls != 0 {
					t.Fatal("failed refresh scheduled redistribution")
				}
				return
			}
			if response.Code != http.StatusOK || r.resetCalls != 1 || r.account != "account:test" || r.event != "event" {
				t.Fatal("reset not scheduled")
			}
		})
	}
}

func TestCodexLeaseControlRedistributionRejectsInvalidRequests(t *testing.T) {
	for _, body := range []string{`{}`, `null`, `{"reset_account_key":"account:test"}`, `{"event_id":"event"}`, `{"reset_account_key":"account:test","event_id":"event","unknown":true}`, `{"reset_account_key":"account:test","event_id":"event"} {}`, `not-json`} {
		t.Run(body, func(t *testing.T) {
			r := &codexLeaseRedistributorTestDouble{}
			response := httptest.NewRecorder()
			(&Server{CodexLeaseRedistributor: r}).handleCodexLeaseRedistribution(response, httptest.NewRequest(http.MethodPost, RuntimeCodexLeaseRedistributionPath, strings.NewReader(body)))
			if response.Code != http.StatusBadRequest || r.calls != 0 || r.resetCalls != 0 {
				t.Fatalf("response=%d, calls=%d/%d", response.Code, r.calls, r.resetCalls)
			}
		})
	}
}

func TestCodexLeaseControlRedistributionFailsClosed(t *testing.T) {
	for _, r := range []CodexLeaseRedistributor{nil, &codexLeaseRedistributorTestDouble{err: errors.New("private failure")}} {
		response := httptest.NewRecorder()
		(&Server{CodexLeaseRedistributor: r}).handleCodexLeaseRedistribution(response, httptest.NewRequest(http.MethodPost, RuntimeCodexLeaseRedistributionPath, nil))
		if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "private failure") {
			t.Fatalf("response=%d %q", response.Code, response.Body.String())
		}
	}
}

func TestCodexLeaseRedistributionRequiresLocalTokenBeforeWorker(t *testing.T) {
	for _, fixture := range []struct {
		name   string
		path   string
		domain NormalCallerDomain
		token  string
		status int
		calls  int
	}{
		{name: "local", domain: NormalCallerLocal, token: "local-token", status: http.StatusNoContent, calls: 1},
		{name: "provider", domain: NormalCallerCodex, token: "provider-token", status: http.StatusForbidden},
		{name: "encoded_claude", path: "/_cq/control/codex/leases/%72edistribute", domain: NormalCallerClaude, token: "provider-token", status: http.StatusForbidden},
		{name: "encoded_codex", path: "/_cq/control/codex/%6ceases/redistribute", domain: NormalCallerCodex, token: "provider-token", status: http.StatusForbidden},
		{name: "unauthenticated", domain: NormalCallerLocal, token: "local-token", status: http.StatusUnauthorized},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			events := []string{}
			worker := &runtimeTestWorker{holder: runtimeHolder("worker"), events: &events}
			supervisor, err := NewRuntimeSupervisor(&runtimeTestListener{}, runtimeHolder("supervisor"), &runtimeTestLauncher{events: &events, workers: []*runtimeTestWorker{worker}}, &runtimeTestCheckpointStore{events: &events})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = supervisor.Boot(context.Background(), WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "artifact"}); err != nil {
				t.Fatal(err)
			}
			if err = supervisor.SetCallerAuthority(testNormalCallerAuthority(t, []NormalCallerCredentialV1{{Domain: fixture.domain, Bearer: fixture.token, SubjectID: "caller"}}, &callerAuthorityTestConsumer{consumed: make(map[string]ProviderBranchAdmissionConsumptionV1)})); err != nil {
				t.Fatal(err)
			}
			path := fixture.path
			if path == "" {
				path = RuntimeCodexLeaseRedistributionPath
			}
			request := httptest.NewRequest(http.MethodPost, path, nil)
			if fixture.name != "unauthenticated" {
				request.Header.Set("Authorization", "Bearer "+fixture.token)
			}
			response := httptest.NewRecorder()
			supervisor.ServeHTTP(response, request)
			calls := 0
			for _, event := range events {
				if event == "execute:worker" {
					calls++
				}
			}
			if response.Code != fixture.status || calls != fixture.calls {
				t.Fatalf("status=%d calls=%d, want=%d/%d", response.Code, calls, fixture.status, fixture.calls)
			}
		})
	}
}

func TestCodexLeaseRedistributionEncodedPathKeepsLocalScope(t *testing.T) {
	for _, path := range []string{"/_cq/control/codex/leases/%72edistribute", "/_cq/control/codex/%6ceases/redistribute", "/_cq/control/codex/leases/%69nvalidate"} {
		request := httptest.NewRequest(http.MethodPost, path, nil)
		if normalCallerPolicy(request) != normalCallerRouteLocal {
			t.Errorf("encoded control path classified outside local scope: %s", path)
		}
		branch, err := NewNormalCallerBranchClassifier(nil)(request.Method, request.RequestURI, nil)
		if err == nil || branch != "" {
			t.Errorf("encoded control path accepted provider branch: %s", path)
		}
	}
}
