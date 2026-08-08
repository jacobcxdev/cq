package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
)

type queuedPrimerUsage struct {
	observations []codex.UsageObservation
	calls        int
}

func (u *queuedPrimerUsage) Read(context.Context, codex.AccountKey) (codex.UsageObservation, error) {
	u.calls++
	index := u.calls - 1
	if index >= len(u.observations) {
		index = len(u.observations) - 1
	}
	return u.observations[index], nil
}

type recordingPrimerRequester struct {
	result PrimerRequestResult
	calls  int
	model  string
}

func (r *recordingPrimerRequester) Send(_ context.Context, _ codex.AccountKey, model string) (PrimerRequestResult, error) {
	r.calls++
	r.model = model
	return r.result, nil
}

func primerObservation(reset time.Time) codex.UsageObservation {
	descriptor := codex.WindowDescriptor{
		RawLimitName: "primary_window", WindowName: quota.Window7Day,
		Period: 7 * 24 * time.Hour, ScopeKind: codex.WindowScopeShared,
		ResetAt: reset, RemainingPct: 100,
	}
	return codex.UsageObservation{
		Result:  quota.Result{Status: quota.StatusOK, Windows: map[quota.WindowName]quota.Window{quota.Window7Day: {RemainingPct: 100, ResetAtUnix: reset.Unix()}}},
		Windows: []codex.WindowDescriptor{descriptor},
	}
}

func testPrimerScheduler(t *testing.T, usage PrimerUsageReader, requester PrimerRequester) (*CodexPrimer, *CodexPrimerStore) {
	t.Helper()
	store, err := OpenCodexPrimerStore(fsutil.NewMemFS(), "/state/primer.json", "/state/primer.key")
	if err != nil {
		t.Fatal(err)
	}
	primer := &CodexPrimer{
		Accounts: func(context.Context) ([]codex.AccountKey, error) { return []codex.AccountKey{"account-1"}, nil },
		Usage:    usage, Requester: requester, Store: store,
		Models: func() []modelregistry.Entry { return primerRegistryEntries() },
	}
	return primer, store
}

func TestCodexPrimerWaitsForFutureReset(t *testing.T) {
	now := time.Unix(1000, 0)
	reset := now.Add(time.Hour)
	usage := &queuedPrimerUsage{observations: []codex.UsageObservation{primerObservation(reset)}}
	requester := &recordingPrimerRequester{result: PrimerRequestResult{State: PrimerRequestAdmitted}}
	primer, _ := testPrimerScheduler(t, usage, requester)

	next, err := primer.RunOnce(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if requester.calls != 0 || !next.Equal(reset) {
		t.Fatalf("request calls/next = %d/%v", requester.calls, next)
	}
}

func TestCodexPrimerDispatchesDueGenerationAndVerifiesEpochAdvance(t *testing.T) {
	now := time.Unix(2000, 0)
	due := now.Add(-time.Second)
	advanced := now.Add(7 * 24 * time.Hour)
	usage := &queuedPrimerUsage{observations: []codex.UsageObservation{primerObservation(due), primerObservation(advanced)}}
	requester := &recordingPrimerRequester{result: PrimerRequestResult{State: PrimerRequestAdmitted}}
	primer, store := testPrimerScheduler(t, usage, requester)

	if _, err := primer.RunOnce(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	oldTarget, _ := PlanCodexPrimerTargets(primerObservation(due).Windows, nil, primerRegistryEntries())
	record, found := store.Lookup("account-1", oldTarget[0])
	if requester.calls != 1 || requester.model != "gpt-5.3-codex-spark" || !found || record.State != PrimerStateVerified {
		t.Fatalf("request/record = calls:%d model:%q found:%v record:%+v", requester.calls, requester.model, found, record)
	}
	if claimed, err := store.Claim("account-1", oldTarget[0]); err != nil || claimed {
		t.Fatalf("verified generation replayed: %v, %v", claimed, err)
	}
}

func TestCodexPrimerMarksUsageActivatedWindowWithoutSyntheticRequest(t *testing.T) {
	now := time.Unix(3000, 0)
	oldReset := now.Add(-time.Second)
	newReset := now.Add(7 * 24 * time.Hour)
	usage := &queuedPrimerUsage{observations: []codex.UsageObservation{primerObservation(newReset)}}
	requester := &recordingPrimerRequester{result: PrimerRequestResult{State: PrimerRequestAdmitted}}
	primer, store := testPrimerScheduler(t, usage, requester)
	oldTarget, _ := PlanCodexPrimerTargets(primerObservation(oldReset).Windows, nil, primerRegistryEntries())
	if err := store.Observe("account-1", oldTarget[0]); err != nil {
		t.Fatal(err)
	}

	if _, err := primer.RunOnce(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	record, found := store.Lookup("account-1", oldTarget[0])
	if requester.calls != 0 || !found || record.State != PrimerStatePrimedExternally {
		t.Fatalf("request/record = calls:%d found:%v record:%+v", requester.calls, found, record)
	}
}

func TestCodexPrimerKeepsAdmittedGenerationVerificationOnlyWithoutEpochAdvance(t *testing.T) {
	now := time.Unix(4000, 0)
	due := now.Add(-time.Second)
	usage := &queuedPrimerUsage{observations: []codex.UsageObservation{primerObservation(due), primerObservation(due)}}
	requester := &recordingPrimerRequester{result: PrimerRequestResult{State: PrimerRequestAdmitted}}
	primer, store := testPrimerScheduler(t, usage, requester)
	targets, _ := PlanCodexPrimerTargets(primerObservation(due).Windows, nil, primerRegistryEntries())

	if _, err := primer.RunOnce(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	record, found := store.Lookup("account-1", targets[0])
	if !found || record.State != PrimerStateVerifying {
		t.Fatalf("record = %+v, %v", record, found)
	}
	if _, err := primer.RunOnce(context.Background(), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if requester.calls != 1 {
		t.Fatalf("admitted generation replayed %d times", requester.calls)
	}
}

func TestCodexPrimerNeverReplaysAmbiguousGeneration(t *testing.T) {
	now := time.Unix(5000, 0)
	due := now.Add(-time.Second)
	usage := &queuedPrimerUsage{observations: []codex.UsageObservation{primerObservation(due), primerObservation(due)}}
	requester := &recordingPrimerRequester{result: PrimerRequestResult{State: PrimerRequestAmbiguous}}
	primer, _ := testPrimerScheduler(t, usage, requester)

	if _, err := primer.RunOnce(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if _, err := primer.RunOnce(context.Background(), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if requester.calls != 1 {
		t.Fatalf("ambiguous generation replayed %d times", requester.calls)
	}
}

type transientPrimerUsage struct {
	calls       atomic.Int32
	observation codex.UsageObservation
}

type failSecondPrimerUsage struct {
	calls       int
	observation codex.UsageObservation
}

func (u *failSecondPrimerUsage) Read(context.Context, codex.AccountKey) (codex.UsageObservation, error) {
	u.calls++
	if u.calls == 2 {
		return codex.UsageObservation{}, errors.New("verification unavailable")
	}
	return u.observation, nil
}

func (u *transientPrimerUsage) Read(context.Context, codex.AccountKey) (codex.UsageObservation, error) {
	if u.calls.Add(1) == 1 {
		return codex.UsageObservation{}, errors.New("temporary usage failure")
	}
	return u.observation, nil
}

type signallingPrimerRequester struct {
	called chan struct{}
}

func (r *signallingPrimerRequester) Send(context.Context, codex.AccountKey, string) (PrimerRequestResult, error) {
	select {
	case r.called <- struct{}{}:
	default:
	}
	return PrimerRequestResult{State: PrimerRequestRejected}, nil
}

func TestCodexPrimerRunRetriesTransientUsageFailure(t *testing.T) {
	now := time.Now()
	usage := &transientPrimerUsage{observation: primerObservation(now.Add(-time.Second))}
	requester := &signallingPrimerRequester{called: make(chan struct{}, 1)}
	primer, _ := testPrimerScheduler(t, usage, requester)
	primer.PollInterval = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- primer.Run(ctx) }()
	select {
	case <-requester.called:
		cancel()
	case <-time.After(time.Second):
		cancel()
		t.Fatal("scheduler stopped after transient usage failure")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop")
	}
}

func TestCodexPrimerDefiniteRejectionRemainsRetryableWhenVerificationFails(t *testing.T) {
	now := time.Unix(5500, 0)
	due := now.Add(-time.Second)
	usage := &failSecondPrimerUsage{observation: primerObservation(due)}
	requester := &recordingPrimerRequester{result: PrimerRequestResult{State: PrimerRequestRejected, HTTPStatus: http.StatusBadRequest}}
	primer, store := testPrimerScheduler(t, usage, requester)
	targets, _ := PlanCodexPrimerTargets(primerObservation(due).Windows, nil, primerRegistryEntries())

	if _, err := primer.RunOnce(context.Background(), now); err == nil {
		t.Fatal("verification failure returned no error")
	}
	record, found := store.Lookup("account-1", targets[0])
	if !found || record.State != PrimerStateRejected {
		t.Fatalf("rejected record became non-retryable: %+v, %v", record, found)
	}
	if _, err := primer.RunOnce(context.Background(), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if requester.calls != 2 {
		t.Fatalf("rejected request calls = %d", requester.calls)
	}
}

type automaticPrimerBackend struct {
	mu            sync.Mutex
	usageCalls    int
	responseCalls int
	due           time.Time
	advanced      time.Time
}

func (b *automaticPrimerBackend) Do(_ context.Context, _ RouteChoice, _ CandidateAttempt, req *http.Request) (*http.Response, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var body string
	switch req.URL.Path {
	case "/backend-api/wham/usage":
		b.usageCalls++
		reset := b.due
		if b.usageCalls > 1 {
			reset = b.advanced
		}
		body = fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":%d}}}`, reset.Unix())
	case "/backend-api/codex/responses":
		b.responseCalls++
		body = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"synthetic\"}}\n\ndata: {\"type\":\"response.completed\"}\n\n"
	default:
		return nil, fmt.Errorf("unexpected path %s", req.URL.Path)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
}

func TestCodexPrimerAutomaticEndToEndFakeBackend(t *testing.T) {
	now := time.Unix(6000, 0)
	backend := &automaticPrimerBackend{due: now.Add(-time.Second), advanced: now.Add(7 * 24 * time.Hour)}
	router := primerTestRouter(backend)
	store, err := OpenCodexPrimerStore(fsutil.NewMemFS(), "/state/primer.json", "/state/primer.key")
	if err != nil {
		t.Fatal(err)
	}
	primer := &CodexPrimer{
		Accounts:  router.AccountKeys,
		Usage:     &CodexPrimerUsageReader{Router: router, UsageURL: "https://chatgpt.example/backend-api/wham/usage"},
		Requester: &CodexPrimerRequester{Router: router, ResponsesURL: "https://chatgpt.example/backend-api/codex/responses"},
		Store:     store,
		Models:    func() []modelregistry.Entry { return primerRegistryEntries() },
	}
	if _, err := primer.RunOnce(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	usageCalls, responseCalls := backend.usageCalls, backend.responseCalls
	backend.mu.Unlock()
	if usageCalls != 2 || responseCalls != 1 {
		t.Fatalf("automatic backend calls = usage:%d responses:%d", usageCalls, responseCalls)
	}
	records := store.Records()
	if len(records) != 2 || records[0].State == PrimerStateClaimed || records[1].State == PrimerStateClaimed {
		t.Fatalf("journal = %+v", records)
	}
	verified := 0
	for _, record := range records {
		if record.State == PrimerStateVerified {
			verified++
		}
	}
	if verified != 1 {
		t.Fatalf("verified records = %d in %+v", verified, records)
	}
}

func TestCodexPrimerModelDisappearanceDoesNotMarkGenerationExternallyPrimed(t *testing.T) {
	now := time.Unix(7000, 0)
	due := now.Add(-time.Second)
	usage := &queuedPrimerUsage{observations: []codex.UsageObservation{primerObservation(due)}}
	requester := &recordingPrimerRequester{result: PrimerRequestResult{State: PrimerRequestAdmitted}}
	primer, store := testPrimerScheduler(t, usage, requester)
	targets, _ := PlanCodexPrimerTargets(primerObservation(due).Windows, nil, primerRegistryEntries())
	if err := store.Observe("account-1", targets[0]); err != nil {
		t.Fatal(err)
	}
	primer.Models = func() []modelregistry.Entry { return nil }

	if _, err := primer.RunOnce(context.Background(), now); err == nil {
		t.Fatal("unresolved model returned no scheduler error")
	}
	record, found := store.Lookup("account-1", targets[0])
	if !found || record.State != PrimerStateObserved || requester.calls != 0 {
		t.Fatalf("record/request = %+v, %v, %d", record, found, requester.calls)
	}
}

func TestCodexPrimerDetectsSlidingUntouchedWindowAndVerifiesStableEpoch(t *testing.T) {
	t0 := time.Unix(8000, 0)
	probe := 5 * time.Second
	firstReset := t0.Add(7 * 24 * time.Hour)
	secondReset := t0.Add(probe).Add(7 * 24 * time.Hour)
	usage := &queuedPrimerUsage{observations: []codex.UsageObservation{
		primerObservation(firstReset),
		primerObservation(secondReset),
		primerObservation(secondReset),
		primerObservation(secondReset),
	}}
	requester := &recordingPrimerRequester{result: PrimerRequestResult{State: PrimerRequestAdmitted}}
	primer, store := testPrimerScheduler(t, usage, requester)
	primer.DormantProbeInterval = probe

	next, err := primer.RunOnce(context.Background(), t0)
	if err != nil {
		t.Fatal(err)
	}
	if requester.calls != 0 || !next.Equal(t0.Add(probe)) {
		t.Fatalf("first sample request/next = %d/%v", requester.calls, next)
	}
	if _, err := primer.RunOnce(context.Background(), t0.Add(probe)); err != nil {
		t.Fatal(err)
	}
	if requester.calls != 1 {
		t.Fatalf("sliding window requests = %d", requester.calls)
	}
	if _, err := primer.RunOnce(context.Background(), t0.Add(2*probe)); err != nil {
		t.Fatal(err)
	}
	verified := 0
	for _, record := range store.Records() {
		if record.State == PrimerStateVerified {
			verified++
		}
	}
	if verified != 1 || requester.calls != 1 {
		t.Fatalf("stable verification/requests = %d/%d records=%+v", verified, requester.calls, store.Records())
	}
}

func TestCodexPrimerDoesNotPrimeStableFreshWindow(t *testing.T) {
	t0 := time.Unix(9000, 0)
	probe := 5 * time.Second
	reset := t0.Add(7 * 24 * time.Hour)
	usage := &queuedPrimerUsage{observations: []codex.UsageObservation{primerObservation(reset), primerObservation(reset)}}
	requester := &recordingPrimerRequester{result: PrimerRequestResult{State: PrimerRequestAdmitted}}
	primer, _ := testPrimerScheduler(t, usage, requester)
	primer.DormantProbeInterval = probe

	if _, err := primer.RunOnce(context.Background(), t0); err != nil {
		t.Fatal(err)
	}
	next, err := primer.RunOnce(context.Background(), t0.Add(probe))
	if err != nil {
		t.Fatal(err)
	}
	if requester.calls != 0 || !next.Equal(reset) {
		t.Fatalf("stable fresh window request/next = %d/%v", requester.calls, next)
	}
}

func TestCodexPrimerNeverReplaysAdmittedDormantWindowWhileEpochStillSlides(t *testing.T) {
	t0 := time.Unix(10000, 0)
	probe := 5 * time.Second
	observations := make([]codex.UsageObservation, 0, 5)
	for _, offset := range []int{0, 1, 1, 2, 3} {
		observedAt := t0.Add(time.Duration(offset) * probe)
		observations = append(observations, primerObservation(observedAt.Add(7*24*time.Hour)))
	}
	usage := &queuedPrimerUsage{observations: observations}
	requester := &recordingPrimerRequester{result: PrimerRequestResult{State: PrimerRequestAdmitted}}
	primer, _ := testPrimerScheduler(t, usage, requester)
	primer.DormantProbeInterval = probe

	for i := 0; i < 4; i++ {
		if _, err := primer.RunOnce(context.Background(), t0.Add(time.Duration(i)*probe)); err != nil {
			t.Fatal(err)
		}
	}
	if requester.calls != 1 {
		t.Fatalf("admitted dormant window replayed %d times", requester.calls)
	}
}

func TestCodexPrimerBoundsRejectedDormantAttemptsAcrossSlidingEpochs(t *testing.T) {
	t0 := time.Unix(11000, 0)
	probe := 5 * time.Second
	observations := make([]codex.UsageObservation, 0, 10)
	for _, offset := range []int{0, 1, 1, 2, 3, 3, 4, 5, 6, 7} {
		observedAt := t0.Add(time.Duration(offset) * probe)
		observations = append(observations, primerObservation(observedAt.Add(7*24*time.Hour)))
	}
	usage := &queuedPrimerUsage{observations: observations}
	requester := &recordingPrimerRequester{result: PrimerRequestResult{State: PrimerRequestRejected, HTTPStatus: http.StatusBadRequest}}
	primer, _ := testPrimerScheduler(t, usage, requester)
	primer.DormantProbeInterval = probe

	for i := 0; i < 8; i++ {
		if _, err := primer.RunOnce(context.Background(), t0.Add(time.Duration(i)*probe)); err != nil {
			t.Fatal(err)
		}
	}
	if requester.calls != codexPrimerMaxRejectedAttempts {
		t.Fatalf("rejected dormant request calls = %d", requester.calls)
	}
}
