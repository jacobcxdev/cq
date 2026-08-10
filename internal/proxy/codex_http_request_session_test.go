package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexHTTPRequestSessionRetriesSameAccountBeforeAdmission(t *testing.T) {
	choice := codexHTTPSessionChoice("account-a")
	first := codexHTTPSessionAttempt("account-a", "candidate-one", "revision-one", 1)
	second := codexHTTPSessionAttempt("account-a", "candidate-two", "revision-two", 2)
	plan := CodexFrozenDispatchPlan{
		status: CodexRoutePlanReady,
		accounts: []CodexFrozenDispatchAccount{{
			choice:   choice,
			attempts: []CandidateAttempt{first, second},
		}},
	}
	frozen, encoded := newCodexHTTPSessionFrozenRequest(t, choice)
	rejectedBody := &codexRejectedTrackingBody{reader: strings.NewReader("rejected")}
	events := make([]string, 0, 8)
	dispatcher := &codexHTTPSessionDispatcher{
		t:      t,
		events: &events,
		outcomes: []codexHTTPSessionOutcome{
			{response: &http.Response{StatusCode: http.StatusUnauthorized, Body: rejectedBody}},
			{response: &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("accepted"))}},
		},
		wantBody: encoded,
	}
	lifecycle := &codexHTTPSessionLifecycle{
		account:      "account-a",
		slotAccounts: map[uint32]codex.AccountKey{1: "account-a", 2: "account-a"},
		events:       &events,
	}
	template, err := http.NewRequest(http.MethodPost, "https://example.invalid/responses", nil)
	if err != nil {
		t.Fatal(err)
	}

	result, err := (&CodexHTTPRequestSession{Executor: dispatcher}).Do(
		context.Background(), template, plan, frozen, lifecycle,
	)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if result.Response == nil || result.Response.StatusCode != http.StatusOK || result.Choice.AccountKey != "account-a" || result.Attempt.Candidate.CandidateID != "candidate-two" {
		t.Fatalf("result = %#v", result)
	}
	if result.Lifecycle == lifecycle || !result.Lifecycle.EverAdmitted() {
		t.Fatalf("latest admitted lifecycle was not transferred: %#v", result.Lifecycle)
	}
	if rejectedBody.readBytes != len("rejected") || rejectedBody.closes != 1 {
		t.Fatalf("discarded rejection read/close = %d/%d, want %d/1", rejectedBody.readBytes, rejectedBody.closes, len("rejected"))
	}
	wantEvents := []string{
		"mark", "send:candidate-one",
		"retry:2",
		"mark", "send:candidate-two",
		"admit",
	}
	if strings.Join(events, ",") != strings.Join(wantEvents, ",") {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
	if _, err := frozen.Replay(); !errors.Is(err, ErrCodexRequestEnvelopeReleased) {
		t.Fatalf("frozen request remained available after Do: %v", err)
	}
}

func TestCodexHTTPRequestSessionRefreshesManagedCandidateOnce(t *testing.T) {
	choice := codexHTTPSessionChoice("account-a")
	direct := codexHTTPSessionAttempt("account-a", "candidate-managed", "revision-one", 1)
	refresh := direct
	plan := CodexFrozenDispatchPlan{
		status: CodexRoutePlanReady,
		accounts: []CodexFrozenDispatchAccount{{
			choice:         choice,
			attempts:       []CandidateAttempt{direct},
			refreshAttempt: &refresh,
		}},
	}
	frozen, encoded := newCodexHTTPSessionFrozenRequest(t, choice)
	events := make([]string, 0, 8)
	dispatcher := &codexHTTPSessionDispatcher{
		t:      t,
		events: &events,
		outcomes: []codexHTTPSessionOutcome{
			{response: &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader("stale"))}},
			{response: &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("accepted"))}},
		},
		wantBody: encoded,
	}
	refresher := &codexHTTPSessionRefresher{
		wantRef:      direct.Candidate,
		wantRevision: direct.Revision,
		ref:          direct.Candidate,
		revision:     "revision-two",
	}
	lifecycle := &codexHTTPSessionLifecycle{
		account:      "account-a",
		slotAccounts: map[uint32]codex.AccountKey{1: "account-a", 2: "account-a"},
		events:       &events,
	}
	template, err := http.NewRequest(http.MethodPost, "https://example.invalid/responses", nil)
	if err != nil {
		t.Fatal(err)
	}

	result, err := (&CodexHTTPRequestSession{Executor: dispatcher, Refresher: refresher}).Do(
		context.Background(), template, plan, frozen, lifecycle,
	)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if result.Response == nil || result.Response.StatusCode != http.StatusOK || result.Attempt.Revision != "revision-two" {
		t.Fatalf("result = %#v", result)
	}
	if refresher.calls != 1 || dispatcher.calls != 2 {
		t.Fatalf("refresh/dispatch calls = %d/%d, want 1/2", refresher.calls, dispatcher.calls)
	}
	wantEvents := []string{"mark", "send:candidate-managed", "retry:2", "mark", "send:candidate-managed", "admit"}
	if strings.Join(events, ",") != strings.Join(wantEvents, ",") {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
}

func TestCodexHTTPRequestSessionAdvancesAccountOnlyForExactHard429(t *testing.T) {
	firstChoice := codexHTTPSessionChoice("account-a")
	secondChoice := codexHTTPSessionChoice("account-b")
	first := codexHTTPSessionAttempt("account-a", "candidate-a", "revision-a", 1)
	second := codexHTTPSessionAttempt("account-b", "candidate-b", "revision-b", 1)
	plan := CodexFrozenDispatchPlan{
		status: CodexRoutePlanReady,
		accounts: []CodexFrozenDispatchAccount{
			{choice: firstChoice, attempts: []CandidateAttempt{first}},
			{choice: secondChoice, attempts: []CandidateAttempt{second}},
		},
	}
	frozen, encoded := newCodexHTTPSessionFrozenRequest(t, firstChoice)
	hardBody := &codexRejectedTrackingBody{reader: strings.NewReader(`{"error":{"type":"usage_limit_reached"}}`)}
	events := make([]string, 0, 8)
	dispatcher := &codexHTTPSessionDispatcher{
		t:      t,
		events: &events,
		outcomes: []codexHTTPSessionOutcome{
			{response: &http.Response{StatusCode: http.StatusTooManyRequests, Body: hardBody}},
			{response: &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("accepted"))}},
		},
		wantBody: encoded,
	}
	lifecycle := &codexHTTPSessionLifecycle{
		account:      "account-a",
		slotAccounts: map[uint32]codex.AccountKey{1: "account-a", 2: "account-b"},
		events:       &events,
	}
	ledger := NewCodexCapacityLedger(nil, 0)
	template, err := http.NewRequest(http.MethodPost, "https://example.invalid/responses", nil)
	if err != nil {
		t.Fatal(err)
	}

	result, err := (&CodexHTTPRequestSession{Executor: dispatcher, Capacity: ledger}).Do(
		context.Background(), template, plan, frozen, lifecycle,
	)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if result.Response == nil || result.Response.StatusCode != http.StatusOK || result.Choice.AccountKey != "account-b" {
		t.Fatalf("result = %#v", result)
	}
	if hardBody.closes != 1 || dispatcher.calls != 2 {
		t.Fatalf("hard response closes/dispatches = %d/%d, want 1/2", hardBody.closes, dispatcher.calls)
	}
	if got := ledger.Capacity("account-a", CapacityBucketBase); got.State != CapacityZero || got.Source != CapacitySourceHardLimit {
		t.Fatalf("hard-limit capacity = %#v", got)
	}
	wantEvents := []string{"mark", "send:candidate-a", "retry:2", "mark", "send:candidate-b", "admit"}
	if strings.Join(events, ",") != strings.Join(wantEvents, ",") {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
}

func TestCodexHTTPRequestSessionSurfacesRetainedEarlyDefaultAfterAlternativesExhaust(t *testing.T) {
	defaultChoice := codexHTTPSessionChoice("account-default")
	ordinaryChoice := codexHTTPSessionChoice("account-ordinary")
	defaultAttempt := codexHTTPSessionAttempt("account-default", "candidate-default", "revision-default", 1)
	ordinaryAttempt := codexHTTPSessionAttempt("account-ordinary", "candidate-ordinary", "revision-ordinary", 1)
	plan := CodexFrozenDispatchPlan{
		status: CodexRoutePlanReady,
		accounts: []CodexFrozenDispatchAccount{
			{choice: defaultChoice, attempts: []CandidateAttempt{defaultAttempt}, isDefault: true},
			{choice: ordinaryChoice, attempts: []CandidateAttempt{ordinaryAttempt}},
		},
	}
	frozen, encoded := newCodexHTTPSessionFrozenRequest(t, defaultChoice)
	defaultBody := &codexRejectedTrackingBody{reader: strings.NewReader("default failure")}
	ordinaryBody := &codexRejectedTrackingBody{reader: strings.NewReader("ordinary failure")}
	events := make([]string, 0, 8)
	dispatcher := &codexHTTPSessionDispatcher{
		t:      t,
		events: &events,
		outcomes: []codexHTTPSessionOutcome{
			{response: &http.Response{StatusCode: http.StatusUnauthorized, Body: defaultBody}},
			{response: &http.Response{StatusCode: http.StatusForbidden, Body: ordinaryBody}},
		},
		wantBody: encoded,
	}
	lifecycle := &codexHTTPSessionLifecycle{
		account:      "account-default",
		slotAccounts: map[uint32]codex.AccountKey{1: "account-default", 2: "account-ordinary"},
		events:       &events,
	}
	template, err := http.NewRequest(http.MethodPost, "https://example.invalid/responses", nil)
	if err != nil {
		t.Fatal(err)
	}

	result, err := (&CodexHTTPRequestSession{Executor: dispatcher}).Do(context.Background(), template, plan, frozen, lifecycle)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if result.Response == nil || result.Choice.AccountKey != "account-default" || result.Attempt.Candidate.CandidateID != "candidate-default" {
		t.Fatalf("result = %#v", result)
	}
	body, readErr := io.ReadAll(result.Response.Body)
	closeErr := result.Response.Body.Close()
	if readErr != nil || closeErr != nil || string(body) != "default failure" {
		t.Fatalf("retained default body = %q, read/close %v/%v", body, readErr, closeErr)
	}
	if defaultBody.closes != 1 || ordinaryBody.readBytes != len("ordinary failure") || ordinaryBody.closes != 1 {
		t.Fatalf("default/ordinary disposal = %d and %d/%d", defaultBody.closes, ordinaryBody.readBytes, ordinaryBody.closes)
	}
	if dispatcher.calls != 2 {
		t.Fatalf("dispatch calls = %d, want 2", dispatcher.calls)
	}
}

func TestCodexHTTPRequestSessionDegradedInventoryOverridesRetainedDefault(t *testing.T) {
	defaultChoice := codexHTTPSessionChoice("account-default")
	ordinaryChoice := codexHTTPSessionChoice("account-ordinary")
	plan := CodexFrozenDispatchPlan{
		status: CodexRoutePlanReady,
		accounts: []CodexFrozenDispatchAccount{
			{
				choice:    defaultChoice,
				attempts:  []CandidateAttempt{codexHTTPSessionAttempt("account-default", "candidate-default", "revision-default", 1)},
				isDefault: true,
			},
			{
				choice:   ordinaryChoice,
				attempts: []CandidateAttempt{codexHTTPSessionAttempt("account-ordinary", "candidate-ordinary", "revision-ordinary", 1)},
			},
		},
	}
	frozen, encoded := newCodexHTTPSessionFrozenRequest(t, defaultChoice)
	defaultBody := &codexRejectedTrackingBody{reader: strings.NewReader("default failure")}
	events := make([]string, 0, 8)
	dispatcher := &codexHTTPSessionDispatcher{
		t:      t,
		events: &events,
		outcomes: []codexHTTPSessionOutcome{
			{response: &http.Response{StatusCode: http.StatusUnauthorized, Body: defaultBody}},
			{preDispatchErr: codex.ErrCredentialInventoryDegraded},
		},
		wantBody: encoded,
	}
	lifecycle := &codexHTTPSessionLifecycle{
		account:      "account-default",
		slotAccounts: map[uint32]codex.AccountKey{1: "account-default", 2: "account-ordinary"},
		events:       &events,
	}
	template, err := http.NewRequest(http.MethodPost, "https://example.invalid/responses", nil)
	if err != nil {
		t.Fatal(err)
	}

	result, err := (&CodexHTTPRequestSession{Executor: dispatcher}).Do(context.Background(), template, plan, frozen, lifecycle)
	if !errors.Is(err, codex.ErrCredentialInventoryDegraded) {
		t.Fatalf("error = %v, want degraded inventory", err)
	}
	if result.Response != nil || result.Lifecycle == lifecycle {
		t.Fatalf("error result retained response or stale lifecycle: %#v", result)
	}
	if defaultBody.closes != 1 || dispatcher.calls != 2 {
		t.Fatalf("retained default close/dispatch calls = %d/%d, want 1/2", defaultBody.closes, dispatcher.calls)
	}
	wantEvents := []string{"mark", "send:candidate-default", "retry:2", "abandon"}
	if strings.Join(events, ",") != strings.Join(wantEvents, ",") {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
}

func TestCodexHTTPAttemptSlotsMatchFrozenDirectAndRefreshOrder(t *testing.T) {
	first := codexHTTPSessionAttempt("account-a", "candidate-one", "revision-one", 1)
	second := codexHTTPSessionAttempt("account-a", "candidate-two", "revision-two", 2)
	refresh := second
	last := codexHTTPSessionAttempt("account-b", "candidate-three", "revision-three", 1)
	plan := CodexFrozenDispatchPlan{
		status: CodexRoutePlanReady,
		accounts: []CodexFrozenDispatchAccount{
			{choice: codexHTTPSessionChoice("account-a"), attempts: []CandidateAttempt{first, second}, refreshAttempt: &refresh},
			{choice: codexHTTPSessionChoice("account-b"), attempts: []CandidateAttempt{last}, isDefault: true},
		},
	}

	slots := CodexHTTPAttemptSlots(plan)
	if len(slots) != 4 {
		t.Fatalf("slots = %d, want 4", len(slots))
	}
	want := []CodexHTTPAttemptSlotPlan{
		{Index: 1, AccountKey: "account-a", CandidateID: "candidate-one", Kind: CodexAttemptSlotDirect},
		{Index: 2, AccountKey: "account-a", CandidateID: "candidate-two", Kind: CodexAttemptSlotDirect},
		{Index: 3, AccountKey: "account-a", CandidateID: "candidate-two", Kind: CodexAttemptSlotEligibleManagedRefresh},
		{Index: 4, AccountKey: "account-b", CandidateID: "candidate-three", Kind: CodexAttemptSlotDirect},
	}
	for index := range want {
		if slots[index] != want[index] {
			t.Fatalf("slot[%d] = %#v, want %#v", index, slots[index], want[index])
		}
	}
	slots[0].AccountKey = "mutated"
	if again := CodexHTTPAttemptSlots(plan); len(again) != 4 || again[0].AccountKey != "account-a" {
		t.Fatalf("slot mutation escaped: %#v", again)
	}
}

func TestCodexAttemptExecutorDispatchFrozenFencesTransportAfterResolution(t *testing.T) {
	identity := codex.AccountIdentity{AccountID: "account", UserID: "user"}
	attempt := CandidateAttempt{
		AccountKey: "identity",
		Candidate: codex.CandidateRef{
			AccountKey:  "identity",
			CandidateID: "candidate",
		},
		Revision: "revision",
		Source:   codex.SourceSystem,
		Identity: identity,
		Ordinal:  1,
	}
	choice := RouteChoice{AccountKey: "identity"}
	fenceErr := errors.New("dispatch fence failed")

	tests := []struct {
		name       string
		resolveErr error
		fenceErr   error
		wantErr    error
		wantFence  int
		wantSend   int
		wantClose  int
	}{
		{name: "dispatch", wantFence: 1, wantSend: 1, wantClose: 1},
		{name: "fence failure", fenceErr: fenceErr, wantErr: fenceErr, wantFence: 1, wantClose: 1},
		{name: "resolution failure", resolveErr: codex.ErrCredentialInventoryDegraded, wantErr: codex.ErrCredentialInventoryDegraded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fenceCalls := 0
			sendCalls := 0
			replayBody := &codexRejectedTrackingBody{reader: strings.NewReader(`{"model":"gpt-5.4"}`)}
			resolver := &testExactSecretResolver{
				materials: map[codex.Revision]codex.CredentialMaterial{
					"revision": testExactCredentialMaterial(identity, "secret"),
				},
				errors: map[codex.Revision]error{"revision": test.resolveErr},
			}
			executor := &CodexAttemptExecutor{
				Secrets: resolver,
				Transport: &CodexTokenTransport{Inner: codexTransportRoundTripFunc(func(request *http.Request) (*http.Response, error) {
					sendCalls++
					if fenceCalls != 1 {
						t.Fatalf("transport crossed before fence: %d", fenceCalls)
					}
					_ = request.Body.Close()
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody}, nil
				})},
			}
			request := makeCodexRequest(`{"model":"gpt-5.4"}`)
			request.GetBody = func() (io.ReadCloser, error) { return replayBody, nil }

			response, actual, dispatched, err := executor.DispatchFrozen(
				context.Background(), choice, attempt, request,
				func(resolved CandidateAttempt) error {
					fenceCalls++
					if resolved != attempt {
						t.Fatalf("resolved attempt = %#v, want %#v", resolved, attempt)
					}
					return test.fenceErr
				},
			)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			if dispatched != (test.wantSend == 1) {
				t.Fatalf("dispatched = %t, want %t", dispatched, test.wantSend == 1)
			}
			if fenceCalls != test.wantFence || sendCalls != test.wantSend {
				t.Fatalf("fence/send calls = %d/%d, want %d/%d", fenceCalls, sendCalls, test.wantFence, test.wantSend)
			}
			if replayBody.closes != test.wantClose {
				t.Fatalf("replay closes = %d, want %d", replayBody.closes, test.wantClose)
			}
			if test.wantSend == 1 {
				if response == nil || actual != attempt {
					t.Fatalf("response/attempt = %#v/%#v", response, actual)
				}
				response.Body.Close()
			} else if response != nil {
				t.Fatalf("response = %#v, want nil", response)
			}
		})
	}
}

func TestCodexHTTPRequestSessionStopsWithoutMigration(t *testing.T) {
	networkErr := errors.New("network failed")
	tests := []struct {
		name         string
		status       int
		header       http.Header
		uncompressed bool
		body         []byte
		dispatchErr  error
		wantErr      error
	}{
		{name: "network", dispatchErr: networkErr, wantErr: networkErr},
		{name: "timeout", dispatchErr: context.DeadlineExceeded, wantErr: context.DeadlineExceeded},
		{name: "server error", status: http.StatusInternalServerError, body: []byte("provider failed")},
		{name: "soft limit", status: http.StatusTooManyRequests, body: []byte(`{"error":{"type":"rate_limit_exceeded"}}`)},
		{name: "encoded hard limit", status: http.StatusTooManyRequests, header: http.Header{"Content-Encoding": {"gzip"}}, body: []byte(`{"error":{"type":"usage_limit_reached"}}`)},
		{name: "already decoded hard limit", status: http.StatusTooManyRequests, uncompressed: true, body: []byte(`{"error":{"type":"usage_limit_reached"}}`)},
		{name: "malformed limit", status: http.StatusTooManyRequests, body: []byte(`{"error":`)},
		{name: "oversize limit", status: http.StatusTooManyRequests, body: bytes.Repeat([]byte("x"), codexAttemptResponseLimit+17)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			firstChoice := codexHTTPSessionChoice("account-a")
			secondChoice := codexHTTPSessionChoice("account-b")
			plan := CodexFrozenDispatchPlan{
				status: CodexRoutePlanReady,
				accounts: []CodexFrozenDispatchAccount{
					{choice: firstChoice, attempts: []CandidateAttempt{codexHTTPSessionAttempt("account-a", "candidate-a", "revision-a", 1)}},
					{choice: secondChoice, attempts: []CandidateAttempt{codexHTTPSessionAttempt("account-b", "candidate-b", "revision-b", 1)}},
				},
			}
			frozen, encoded := newCodexHTTPSessionFrozenRequest(t, firstChoice)
			events := make([]string, 0, 8)
			outcome := codexHTTPSessionOutcome{err: test.dispatchErr}
			var response *http.Response
			if test.dispatchErr == nil {
				response = &http.Response{
					StatusCode:   test.status,
					Header:       test.header,
					Body:         io.NopCloser(bytes.NewReader(test.body)),
					Uncompressed: test.uncompressed,
				}
				outcome.response = response
			}
			dispatcher := &codexHTTPSessionDispatcher{
				t: t, events: &events, wantBody: encoded,
				outcomes: []codexHTTPSessionOutcome{
					outcome,
					{response: &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}},
				},
			}
			lifecycle := &codexHTTPSessionLifecycle{
				account: "account-a", slotAccounts: map[uint32]codex.AccountKey{1: "account-a", 2: "account-b"}, events: &events,
			}
			template, err := http.NewRequest(http.MethodPost, "https://example.invalid/responses", nil)
			if err != nil {
				t.Fatal(err)
			}

			result, err := (&CodexHTTPRequestSession{Executor: dispatcher}).Do(context.Background(), template, plan, frozen, lifecycle)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			if dispatcher.calls != 1 {
				t.Fatalf("dispatch calls = %d, want 1", dispatcher.calls)
			}
			if test.dispatchErr != nil {
				if result.Response != nil {
					t.Fatalf("response = %#v, want nil", result.Response)
				}
				wantEvents := []string{"mark", "send:candidate-a", "indeterminate", "drain"}
				if strings.Join(events, ",") != strings.Join(wantEvents, ",") {
					t.Fatalf("events = %v, want %v", events, wantEvents)
				}
				return
			}
			if result.Response != response {
				t.Fatalf("response = %#v, want original response", result.Response)
			}
			got, readErr := io.ReadAll(result.Response.Body)
			closeErr := result.Response.Body.Close()
			if readErr != nil || closeErr != nil || !bytes.Equal(got, test.body) {
				t.Fatalf("body length/read/close = %d/%v/%v, want %d", len(got), readErr, closeErr, len(test.body))
			}
			wantEvents := []string{"mark", "send:candidate-a", "finish"}
			if strings.Join(events, ",") != strings.Join(wantEvents, ",") {
				t.Fatalf("events = %v, want %v", events, wantEvents)
			}
		})
	}
}

func TestCodexHTTPRequestSessionAbandonsWhenDispatchFenceFails(t *testing.T) {
	choice := codexHTTPSessionChoice("account-a")
	plan := CodexFrozenDispatchPlan{
		status: CodexRoutePlanReady,
		accounts: []CodexFrozenDispatchAccount{{
			choice: choice, attempts: []CandidateAttempt{codexHTTPSessionAttempt("account-a", "candidate-a", "revision-a", 1)},
		}},
	}
	frozen, encoded := newCodexHTTPSessionFrozenRequest(t, choice)
	events := make([]string, 0, 4)
	dispatcher := &codexHTTPSessionDispatcher{
		t: t, events: &events, wantBody: encoded,
		outcomes: []codexHTTPSessionOutcome{{response: &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}}},
	}
	fenceErr := errors.New("journal fence failed")
	lifecycle := &codexHTTPSessionLifecycle{
		account: "account-a", slotAccounts: map[uint32]codex.AccountKey{1: "account-a"}, events: &events, markErr: fenceErr,
	}
	template, err := http.NewRequest(http.MethodPost, "https://example.invalid/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := (&CodexHTTPRequestSession{Executor: dispatcher}).Do(canceled, template, plan, frozen, lifecycle)
	if !errors.Is(err, fenceErr) {
		t.Fatalf("error = %v, want fence failure", err)
	}
	if result.Response != nil || result.Lifecycle == lifecycle {
		t.Fatalf("result = %#v, want latest abandoned lifecycle and no response", result)
	}
	if dispatcher.calls != 1 {
		t.Fatalf("dispatch calls = %d, want one resolution and zero sends", dispatcher.calls)
	}
	wantEvents := []string{"mark", "abandon"}
	if strings.Join(events, ",") != strings.Join(wantEvents, ",") {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
	if got := result.Lifecycle.(*codexHTTPSessionLifecycle).abandonContextErr; got != nil {
		t.Fatalf("abandon context error = %v, want cleanup context", got)
	}
}

func TestCodexHTTPRequestSessionDrainsWhenRetryTransitionFails(t *testing.T) {
	choice := codexHTTPSessionChoice("account-a")
	plan := CodexFrozenDispatchPlan{
		status: CodexRoutePlanReady,
		accounts: []CodexFrozenDispatchAccount{{
			choice: choice,
			attempts: []CandidateAttempt{
				codexHTTPSessionAttempt("account-a", "candidate-a", "revision-a", 1),
				codexHTTPSessionAttempt("account-a", "candidate-b", "revision-b", 2),
			},
		}},
	}
	frozen, encoded := newCodexHTTPSessionFrozenRequest(t, choice)
	events := make([]string, 0, 8)
	rejectedBody := &codexRejectedTrackingBody{reader: strings.NewReader("rejected")}
	dispatcher := &codexHTTPSessionDispatcher{
		t: t, events: &events, wantBody: encoded,
		outcomes: []codexHTTPSessionOutcome{{response: &http.Response{StatusCode: http.StatusUnauthorized, Body: rejectedBody}}},
	}
	lifecycle := &codexHTTPSessionLifecycle{
		account: "account-a", slotAccounts: map[uint32]codex.AccountKey{1: "account-a", 2: "account-a"}, events: &events,
		retryErr: context.Canceled,
	}
	template, err := http.NewRequest(http.MethodPost, "https://example.invalid/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := (&CodexHTTPRequestSession{Executor: dispatcher}).Do(canceled, template, plan, frozen, lifecycle)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want cancellation", err)
	}
	if result.Response != nil || result.Lifecycle == lifecycle {
		t.Fatalf("result = %#v, want latest drained lifecycle and no response", result)
	}
	if rejectedBody.closes != 1 {
		t.Fatalf("rejected body closes = %d, want 1", rejectedBody.closes)
	}
	wantEvents := []string{"mark", "send:candidate-a", "retry:2", "indeterminate", "drain"}
	if strings.Join(events, ",") != strings.Join(wantEvents, ",") {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
	if got := result.Lifecycle.(*codexHTTPSessionLifecycle).indeterminateContextErr; got != nil {
		t.Fatalf("indeterminate context error = %v, want cleanup context", got)
	}
}

func TestCodexHTTPRequestSessionNeverMigratesAfterPriorAdmission(t *testing.T) {
	firstChoice := codexHTTPSessionChoice("account-a")
	secondChoice := codexHTTPSessionChoice("account-b")
	plan := CodexFrozenDispatchPlan{
		status: CodexRoutePlanReady,
		accounts: []CodexFrozenDispatchAccount{
			{choice: firstChoice, attempts: []CandidateAttempt{codexHTTPSessionAttempt("account-a", "candidate-a", "revision-a", 1)}},
			{choice: secondChoice, attempts: []CandidateAttempt{codexHTTPSessionAttempt("account-b", "candidate-b", "revision-b", 1)}},
		},
	}
	frozen, encoded := newCodexHTTPSessionFrozenRequest(t, firstChoice)
	events := make([]string, 0, 4)
	hardBody := []byte(`{"error":{"type":"usage_limit_reached"}}`)
	response := &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(bytes.NewReader(hardBody))}
	dispatcher := &codexHTTPSessionDispatcher{
		t: t, events: &events, wantBody: encoded,
		outcomes: []codexHTTPSessionOutcome{
			{response: response},
			{response: &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}},
		},
	}
	lifecycle := &codexHTTPSessionLifecycle{
		account: "account-a", admitted: true,
		slotAccounts: map[uint32]codex.AccountKey{1: "account-a", 2: "account-b"}, events: &events,
	}
	template, err := http.NewRequest(http.MethodPost, "https://example.invalid/responses", nil)
	if err != nil {
		t.Fatal(err)
	}

	result, err := (&CodexHTTPRequestSession{Executor: dispatcher}).Do(context.Background(), template, plan, frozen, lifecycle)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if result.Response != response || result.Choice.AccountKey != "account-a" || dispatcher.calls != 1 {
		t.Fatalf("result/calls = %#v/%d, want original A response and one dispatch", result, dispatcher.calls)
	}
	got, readErr := io.ReadAll(result.Response.Body)
	closeErr := result.Response.Body.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(got, hardBody) {
		t.Fatalf("body = %q, read/close %v/%v", got, readErr, closeErr)
	}
}

func TestCodexHTTPRequestSessionCommitsAdmissionEvidenceBeforeExposure(t *testing.T) {
	choice := codexHTTPSessionChoice("account-a")
	plan := CodexFrozenDispatchPlan{
		status: CodexRoutePlanReady,
		accounts: []CodexFrozenDispatchAccount{{
			choice: choice, attempts: []CandidateAttempt{codexHTTPSessionAttempt("account-a", "candidate-a", "revision-a", 1)},
		}},
	}
	frozen, encoded := newCodexHTTPSessionFrozenRequest(t, choice)
	events := make([]string, 0, 6)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"X-Codex-Turn-State": {"private-state"}},
		Body:       http.NoBody,
	}
	dispatcher := &codexHTTPSessionDispatcher{
		t: t, events: &events, wantBody: encoded,
		outcomes: []codexHTTPSessionOutcome{{response: response}},
	}
	lifecycle := &codexHTTPSessionLifecycle{account: "account-a", slotAccounts: map[uint32]codex.AccountKey{1: "account-a"}, events: &events}
	template, err := http.NewRequest(http.MethodPost, "https://example.invalid/responses", nil)
	if err != nil {
		t.Fatal(err)
	}

	result, err := (&CodexHTTPRequestSession{Executor: dispatcher}).Do(context.Background(), template, plan, frozen, lifecycle)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if result.Response != response || !result.Lifecycle.EverAdmitted() {
		t.Fatalf("result = %#v, want admitted response", result)
	}
	latest := result.Lifecycle.(*codexHTTPSessionLifecycle)
	if latest.lastAdmission != (CodexHTTPAdmissionEvidence{TurnState: "private-state", HasTurnState: true}) {
		t.Fatalf("admission evidence = %#v", latest.lastAdmission)
	}
	completed, err := result.Lifecycle.ProviderCompleted(CodexHTTPCompletionEvidence{EndTurn: true})
	if err != nil || completed == result.Lifecycle {
		t.Fatalf("observer completion = %#v, %v", completed, err)
	}
	response.Body.Close()
}

func TestCodexHTTPRequestSessionOrphansFailedOrMalformedAdmission(t *testing.T) {
	admissionErr := errors.New("admission commit failed")
	for _, test := range []struct {
		name      string
		header    http.Header
		admitErr  error
		wantErr   error
		wantAdmit bool
	}{
		{name: "admission failure", admitErr: admissionErr, wantErr: admissionErr},
		{
			name:      "malformed turn state",
			header:    http.Header{"X-Codex-Turn-State": {"state-a", "state-b"}},
			wantAdmit: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			choice := codexHTTPSessionChoice("account-a")
			plan := CodexFrozenDispatchPlan{
				status: CodexRoutePlanReady,
				accounts: []CodexFrozenDispatchAccount{{
					choice: choice, attempts: []CandidateAttempt{codexHTTPSessionAttempt("account-a", "candidate-a", "revision-a", 1)},
				}},
			}
			frozen, encoded := newCodexHTTPSessionFrozenRequest(t, choice)
			events := make([]string, 0, 8)
			body := &codexRejectedTrackingBody{reader: strings.NewReader("accepted but not exposed")}
			dispatcher := &codexHTTPSessionDispatcher{
				t: t, events: &events, wantBody: encoded,
				outcomes: []codexHTTPSessionOutcome{{response: &http.Response{StatusCode: http.StatusOK, Header: test.header, Body: body}}},
			}
			lifecycle := &codexHTTPSessionLifecycle{
				account: "account-a", slotAccounts: map[uint32]codex.AccountKey{1: "account-a"}, events: &events, admitErr: test.admitErr,
			}
			template, err := http.NewRequest(http.MethodPost, "https://example.invalid/responses", nil)
			if err != nil {
				t.Fatal(err)
			}

			result, err := (&CodexHTTPRequestSession{Executor: dispatcher}).Do(context.Background(), template, plan, frozen, lifecycle)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("error = %v, want %v", err, test.wantErr)
				}
			} else if err == nil || !strings.Contains(err.Error(), "conflicting Codex turn state headers") {
				t.Fatalf("error = %v, want malformed turn state", err)
			}
			if result.Response != nil || result.Lifecycle.EverAdmitted() != test.wantAdmit {
				t.Fatalf("result = %#v, want admitted %t and no response", result, test.wantAdmit)
			}
			if body.readBytes != len("accepted but not exposed") || body.closes != 1 {
				t.Fatalf("body disposal = %d/%d", body.readBytes, body.closes)
			}
			wantEvents := []string{"mark", "send:candidate-a", "admit", "indeterminate", "drain"}
			if strings.Join(events, ",") != strings.Join(wantEvents, ",") {
				t.Fatalf("events = %v, want %v", events, wantEvents)
			}
		})
	}
}

func TestCodexHTTPRequestSessionReturnsTypedDefaultFailureAfterExhaustion(t *testing.T) {
	choice := codexHTTPSessionChoice("account-a")
	plan := CodexFrozenDispatchPlan{
		status: CodexRoutePlanDefaultMissing,
		accounts: []CodexFrozenDispatchAccount{{
			choice: choice, attempts: []CandidateAttempt{codexHTTPSessionAttempt("account-a", "candidate-a", "revision-a", 1)},
		}},
	}
	frozen, encoded := newCodexHTTPSessionFrozenRequest(t, choice)
	rejectedBody := &codexRejectedTrackingBody{reader: strings.NewReader("rejected")}
	events := make([]string, 0, 4)
	dispatcher := &codexHTTPSessionDispatcher{
		t: t, events: &events, wantBody: encoded,
		outcomes: []codexHTTPSessionOutcome{{response: &http.Response{StatusCode: http.StatusUnauthorized, Body: rejectedBody}}},
	}
	lifecycle := &codexHTTPSessionLifecycle{account: "account-a", slotAccounts: map[uint32]codex.AccountKey{1: "account-a"}, events: &events}
	template, err := http.NewRequest(http.MethodPost, "https://example.invalid/responses", nil)
	if err != nil {
		t.Fatal(err)
	}

	result, err := (&CodexHTTPRequestSession{Executor: dispatcher}).Do(context.Background(), template, plan, frozen, lifecycle)
	var policyErr *CodexRoutePolicyError
	if !errors.As(err, &policyErr) || policyErr.Status != CodexRoutePlanDefaultMissing {
		t.Fatalf("error = %v, want typed missing default", err)
	}
	if result.Response != nil || rejectedBody.readBytes != len("rejected") || rejectedBody.closes != 1 {
		t.Fatalf("result/body disposal = %#v and %d/%d", result, rejectedBody.readBytes, rejectedBody.closes)
	}
	wantEvents := []string{"mark", "send:candidate-a", "finish"}
	if strings.Join(events, ",") != strings.Join(wantEvents, ",") {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
}

func TestCodexHTTPRequestSessionRefreshDegradationWinsAndStaleRevisionSkips(t *testing.T) {
	for _, test := range []struct {
		name         string
		refreshRef   codex.CandidateRef
		refreshRev   codex.Revision
		refreshErr   error
		wantErr      error
		wantDispatch int
	}{
		{name: "degraded inventory wins", refreshErr: codex.ErrCredentialInventoryDegraded, wantErr: codex.ErrCredentialInventoryDegraded, wantDispatch: 1},
		{name: "stale revision skips refresh", refreshRev: "revision-a", wantDispatch: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			firstChoice := codexHTTPSessionChoice("account-a")
			secondChoice := codexHTTPSessionChoice("account-b")
			direct := codexHTTPSessionAttempt("account-a", "candidate-a", "revision-a", 1)
			refresh := direct
			plan := CodexFrozenDispatchPlan{
				status: CodexRoutePlanReady,
				accounts: []CodexFrozenDispatchAccount{
					{choice: firstChoice, attempts: []CandidateAttempt{direct}, refreshAttempt: &refresh},
					{choice: secondChoice, attempts: []CandidateAttempt{codexHTTPSessionAttempt("account-b", "candidate-b", "revision-b", 1)}},
				},
			}
			frozen, encoded := newCodexHTTPSessionFrozenRequest(t, firstChoice)
			events := make([]string, 0, 8)
			dispatcher := &codexHTTPSessionDispatcher{
				t: t, events: &events, wantBody: encoded,
				outcomes: []codexHTTPSessionOutcome{
					{response: &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader("rejected"))}},
					{response: &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}},
				},
			}
			refreshRef := test.refreshRef
			if refreshRef.AccountKey == "" {
				refreshRef = direct.Candidate
			}
			refresher := &codexHTTPSessionRefresher{
				wantRef: direct.Candidate, wantRevision: direct.Revision,
				ref: refreshRef, revision: test.refreshRev, err: test.refreshErr,
			}
			lifecycle := &codexHTTPSessionLifecycle{
				account:      "account-a",
				slotAccounts: map[uint32]codex.AccountKey{1: "account-a", 2: "account-a", 3: "account-b"}, events: &events,
			}
			template, err := http.NewRequest(http.MethodPost, "https://example.invalid/responses", nil)
			if err != nil {
				t.Fatal(err)
			}

			result, err := (&CodexHTTPRequestSession{Executor: dispatcher, Refresher: refresher}).Do(context.Background(), template, plan, frozen, lifecycle)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			if dispatcher.calls != test.wantDispatch || refresher.calls != 1 {
				t.Fatalf("dispatch/refresh calls = %d/%d, want %d/1", dispatcher.calls, refresher.calls, test.wantDispatch)
			}
			if test.wantErr != nil {
				if result.Response != nil {
					t.Fatalf("response = %#v, want nil", result.Response)
				}
				return
			}
			if result.Response == nil || result.Response.StatusCode != http.StatusOK || result.Choice.AccountKey != "account-b" {
				t.Fatalf("result = %#v", result)
			}
			result.Response.Body.Close()
		})
	}
}

type codexHTTPSessionOutcome struct {
	response       *http.Response
	err            error
	preDispatchErr error
}

type codexHTTPSessionDispatcher struct {
	t        *testing.T
	outcomes []codexHTTPSessionOutcome
	calls    int
	events   *[]string
	wantBody []byte
}

type codexHTTPSessionRefresher struct {
	wantRef      codex.CandidateRef
	wantRevision codex.Revision
	ref          codex.CandidateRef
	revision     codex.Revision
	err          error
	calls        int
}

func (refresher *codexHTTPSessionRefresher) RefreshReference(_ context.Context, ref codex.CandidateRef, revision codex.Revision) (codex.CandidateRef, codex.Revision, error) {
	refresher.calls++
	if ref != refresher.wantRef || revision != refresher.wantRevision {
		return codex.CandidateRef{}, "", errors.New("unexpected refresh input")
	}
	return refresher.ref, refresher.revision, refresher.err
}

func (dispatcher *codexHTTPSessionDispatcher) DispatchFrozen(
	ctx context.Context,
	_ RouteChoice,
	attempt CandidateAttempt,
	request *http.Request,
	markDispatched func(CandidateAttempt) error,
) (*http.Response, CandidateAttempt, bool, error) {
	dispatcher.t.Helper()
	if dispatcher.calls >= len(dispatcher.outcomes) {
		dispatcher.t.Fatalf("unexpected dispatch %d", dispatcher.calls+1)
	}
	outcome := dispatcher.outcomes[dispatcher.calls]
	dispatcher.calls++
	if outcome.preDispatchErr != nil {
		return nil, attempt, false, outcome.preDispatchErr
	}
	body, err := request.GetBody()
	if err != nil {
		dispatcher.t.Fatalf("GetBody: %v", err)
	}
	got, readErr := io.ReadAll(body)
	closeErr := body.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(got, dispatcher.wantBody) {
		dispatcher.t.Fatalf("frozen request body = %q, read/close %v/%v, want %q", got, readErr, closeErr, dispatcher.wantBody)
	}
	if err := markDispatched(attempt); err != nil {
		return nil, attempt, false, err
	}
	*dispatcher.events = append(*dispatcher.events, "send:"+string(attempt.Candidate.CandidateID))
	return outcome.response, attempt, true, outcome.err
}

type codexHTTPSessionLifecycle struct {
	account                 codex.AccountKey
	admitted                bool
	slotAccounts            map[uint32]codex.AccountKey
	events                  *[]string
	markErr                 error
	retryErr                error
	admitErr                error
	abandonContextErr       error
	indeterminateContextErr error
	lastAdmission           CodexHTTPAdmissionEvidence
	providerCompletedAt     bool
}

func (lifecycle *codexHTTPSessionLifecycle) copy() *codexHTTPSessionLifecycle {
	copy := *lifecycle
	return &copy
}

func (lifecycle *codexHTTPSessionLifecycle) EverAdmitted() bool { return lifecycle.admitted }
func (lifecycle *codexHTTPSessionLifecycle) AccountKey() codex.AccountKey {
	return lifecycle.account
}

func (lifecycle *codexHTTPSessionLifecycle) MarkDispatchedContext(context.Context) (CodexHTTPRequestLifecycle, error) {
	next := lifecycle.copy()
	*next.events = append(*next.events, "mark")
	if next.markErr != nil {
		return nil, next.markErr
	}
	return next, nil
}

func (lifecycle *codexHTTPSessionLifecycle) RejectAndPrepareContext(_ context.Context, slot uint32) (CodexHTTPRequestLifecycle, error) {
	next := lifecycle.copy()
	account, ok := next.slotAccounts[slot]
	if !ok {
		return nil, errors.New("unknown slot")
	}
	next.account = account
	*next.events = append(*next.events, "retry:"+string(rune('0'+slot)))
	if next.retryErr != nil {
		return nil, next.retryErr
	}
	return next, nil
}

func (lifecycle *codexHTTPSessionLifecycle) AbandonBeforeDispatchContext(ctx context.Context) (CodexHTTPRequestLifecycle, error) {
	next := lifecycle.copy()
	next.abandonContextErr = ctx.Err()
	*next.events = append(*next.events, "abandon")
	return next, nil
}

func (lifecycle *codexHTTPSessionLifecycle) FinishRejected() (CodexHTTPRequestLifecycle, error) {
	next := lifecycle.copy()
	*next.events = append(*next.events, "finish")
	return next, nil
}

func (lifecycle *codexHTTPSessionLifecycle) IndeterminateContext(ctx context.Context, _ CodexHTTPResponseEvidence) (CodexHTTPRequestLifecycle, error) {
	next := lifecycle.copy()
	next.indeterminateContextErr = ctx.Err()
	*next.events = append(*next.events, "indeterminate")
	return next, nil
}

func (lifecycle *codexHTTPSessionLifecycle) Drain() (CodexHTTPRequestLifecycle, error) {
	next := lifecycle.copy()
	*next.events = append(*next.events, "drain")
	return next, nil
}

func (lifecycle *codexHTTPSessionLifecycle) AdmitHTTP2xxContext(_ context.Context, evidence CodexHTTPAdmissionEvidence) (CodexHTTPRequestLifecycle, error) {
	next := lifecycle.copy()
	next.lastAdmission = evidence
	*next.events = append(*next.events, "admit")
	if next.admitErr != nil {
		return nil, next.admitErr
	}
	next.admitted = true
	return next, nil
}

func (lifecycle *codexHTTPSessionLifecycle) ProviderCompleted(evidence CodexHTTPCompletionEvidence) (CodexHTTPRequestLifecycle, error) {
	next := lifecycle.copy()
	next.providerCompletedAt = evidence.EndTurn
	*next.events = append(*next.events, "complete")
	return next, nil
}

func (lifecycle *codexHTTPSessionLifecycle) ProviderFailed(CodexHTTPResponseEvidence) (CodexHTTPRequestLifecycle, error) {
	next := lifecycle.copy()
	*next.events = append(*next.events, "provider-failed")
	return next, nil
}

func codexHTTPSessionChoice(account codex.AccountKey) RouteChoice {
	return RouteChoice{
		AccountKey:      account,
		RequestedModel:  "gpt-5.4",
		EffectiveModel:  "gpt-5.4",
		RequiredBuckets: []CapacityBucket{CapacityBucketBase},
	}
}

func codexHTTPSessionAttempt(account codex.AccountKey, candidate codex.CandidateID, revision codex.Revision, ordinal int) CandidateAttempt {
	return CandidateAttempt{
		AccountKey: account,
		Candidate:  codex.CandidateRef{AccountKey: account, CandidateID: candidate},
		Revision:   revision,
		Source:     codex.SourceManaged,
		Ordinal:    ordinal,
	}
}

func newCodexHTTPSessionFrozenRequest(t *testing.T, choice RouteChoice) (*CodexFrozenRequest, []byte) {
	t.Helper()
	body := frozenRequestBody(choice.RequestedModel, CodexRequestTurn, "private prompt")
	inspection, err := InspectCodexNativeRequest(context.Background(), body, http.Header{
		"Content-Type":  {"application/json"},
		"Accept":        {"text/event-stream"},
		"Authorization": {"Bearer private"},
	})
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := inspection.Freeze(context.Background(), choice, nil, HeadroomModeCache)
	if err != nil {
		t.Fatal(err)
	}
	return frozen, body
}
