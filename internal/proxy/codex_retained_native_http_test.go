package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexRetainedNativeHTTPDeclineRestoresExactRequest(t *testing.T) {
	t.Parallel()
	body := frozenRequestBody("gpt-5", CodexRequestTurn, "private-body")
	original := &codexRetainedHTTPTestBody{Reader: bytes.NewReader(body)}
	planner := &codexRetainedHTTPPlannerStub{mutateProbe: true}
	handler := newCodexRetainedHTTPTestHandler(t, planner)
	request := httptest.NewRequest(http.MethodPost, "http://localhost/v1/responses", original)
	originalGetBodyCalls := 0
	request.GetBody = func() (io.ReadCloser, error) {
		originalGetBodyCalls++
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	request.ContentLength = int64(len(body))
	request.Header = http.Header{"X-Private": {"unchanged"}}
	wantHeaders := request.Header.Clone()
	wantLength := request.ContentLength

	handled, model := handler.TryServe(httptest.NewRecorder(), request, false)
	if handled || model != "" || planner.probeCalls != 1 || planner.buildCalls != 0 {
		t.Fatalf("decline = handled %t model %q probe/build %d/%d", handled, model, planner.probeCalls, planner.buildCalls)
	}
	if !reflect.DeepEqual(request.Header, wantHeaders) || request.ContentLength != wantLength || original.closes != 0 {
		t.Fatalf("request metadata changed: headers=%#v length=%d closes=%d", request.Header, request.ContentLength, original.closes)
	}
	replay, ok := request.Body.(*codexRetainedReplayBody)
	if !ok || replay.state == nil {
		t.Fatalf("restored body = %T, want retained replay ownership", request.Body)
	}
	buffered := replay.state.buffered
	getBody, err := request.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	assertCodexRetainedHTTPBody(t, request.Body, body)
	if original.closes != 1 {
		t.Fatalf("original closes = %d, want 1 after fallback ownership", original.closes)
	}
	if replay.state.buffered == nil {
		t.Fatal("restored buffer released while a GetBody replay still owned it")
	}
	assertCodexRetainedHTTPBody(t, getBody, body)
	if replay.state.buffered != nil || bytes.Count(buffered, []byte{0}) != len(buffered) {
		t.Fatal("restored private buffer remained reachable after all replay bodies closed")
	}
	restoredBody, err := request.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	assertCodexRetainedHTTPBody(t, restoredBody, body)
	if originalGetBodyCalls != 1 {
		t.Fatalf("original GetBody calls = %d, want 1 after retained buffer release", originalGetBodyCalls)
	}
}

func TestCodexRetainedNativeHTTPOversizeDeclineReplaysPrefixAndTail(t *testing.T) {
	t.Parallel()
	body := bytes.Repeat([]byte("private-oversize-pattern"), maxRequestBody/24+2)
	original := &codexRetainedHTTPTestBody{Reader: bytes.NewReader(body)}
	planner := &codexRetainedHTTPPlannerStub{}
	handler := newCodexRetainedHTTPTestHandler(t, planner)
	request := httptest.NewRequest(http.MethodPost, "http://localhost/v1/responses", original)
	request.GetBody = nil

	handled, _ := handler.TryServe(httptest.NewRecorder(), request, false)
	if handled || planner.probeCalls != 0 || planner.buildCalls != 0 || request.GetBody != nil || original.closes != 0 {
		t.Fatalf("oversize decline = handled %t probe/build %d/%d getbody=%t closes=%d", handled, planner.probeCalls, planner.buildCalls, request.GetBody != nil, original.closes)
	}
	replay, ok := request.Body.(*codexRetainedReplayBody)
	if !ok || replay.state == nil {
		t.Fatalf("oversize body = %T, want retained replay ownership", request.Body)
	}
	prefix := replay.state.buffered
	assertCodexRetainedHTTPBody(t, request.Body, body)
	if original.closes != 1 {
		t.Fatalf("original closes = %d, want 1", original.closes)
	}
	if replay.state.buffered != nil || bytes.Count(prefix, []byte{0}) != len(prefix) {
		t.Fatal("oversize private prefix remained reachable after fallback body closed")
	}
}

func TestCodexRetainedNativeHTTPClaimCarriesExactBound(t *testing.T) {
	t.Parallel()
	body := frozenRequestBody("gpt-5", CodexRequestTurn, "private-body")
	original := &codexRetainedHTTPTestBody{Reader: bytes.NewReader(body)}
	expected := &CodexLeaseBoundExpectation{
		Identity:         CodexJournalRecordIdentity{LaneDigest: "lane", TurnDigest: "turn", ModeEpoch: 7, Authoritative: true},
		AccountKey:       "account-a",
		RecordGeneration: 12,
	}
	planner := &codexRetainedHTTPPlannerStub{expected: expected, claimed: true, buildErr: errors.New("private-build-error")}
	handler := newCodexRetainedHTTPTestHandler(t, planner)
	request := httptest.NewRequest(http.MethodPost, "http://localhost/v1/responses", original)
	writer := httptest.NewRecorder()

	handled, _ := handler.TryServe(writer, request, false)
	if !handled || planner.probeCalls != 1 || planner.buildCalls != 1 || planner.buildExpected == nil || *planner.buildExpected != *expected {
		t.Fatalf("claim = handled %t probe/build %d/%d expected %#v", handled, planner.probeCalls, planner.buildCalls, planner.buildExpected)
	}
	if !bytes.Equal(planner.buildEncoded, body) || original.closes != 1 || writer.Code != http.StatusServiceUnavailable {
		t.Fatalf("claim body/close/status = equal %t closes %d status %d", bytes.Equal(planner.buildEncoded, body), original.closes, writer.Code)
	}
}

func TestCodexRetainedNativeHTTPClaimJoinsNativeAdmission(t *testing.T) {
	body := frozenRequestBody("gpt-5", CodexRequestTurn, "private-body")
	buildStarted := make(chan struct{})
	buildRelease := make(chan struct{})
	planner := &codexRetainedHTTPPlannerStub{
		expected: &CodexLeaseBoundExpectation{
			Identity:         CodexJournalRecordIdentity{LaneDigest: "lane", TurnDigest: "turn", ModeEpoch: 7, Authoritative: true},
			AccountKey:       "account-a",
			RecordGeneration: 12,
		},
		claimed:      true,
		buildErr:     errors.New("stop after planning"),
		buildStarted: buildStarted,
		buildRelease: buildRelease,
	}
	handler := newCodexRetainedHTTPTestHandler(t, planner)
	served := make(chan struct{})
	go func() {
		defer close(served)
		handler.TryServe(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "http://localhost/v1/responses", bytes.NewReader(body)), false)
	}()
	select {
	case <-buildStarted:
	case <-time.After(time.Second):
		t.Fatal("claimed retained request did not begin building")
	}

	drainContext, cancelDrain := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelDrain()
	if err := handler.native.CloseAndDrain(drainContext); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("claimed retained drain = %v, want deadline", err)
	}
	close(buildRelease)
	select {
	case <-served:
	case <-time.After(time.Second):
		t.Fatal("claimed retained request did not return")
	}
	if err := handler.native.CloseAndDrain(context.Background()); err != nil {
		t.Fatalf("claimed retained final drain: %v", err)
	}
}

func TestCodexRetainedNativeHTTPClosedAdmissionRejectsClaimBeforeBuild(t *testing.T) {
	body := frozenRequestBody("gpt-5", CodexRequestTurn, "private-body")
	planner := &codexRetainedHTTPPlannerStub{
		expected: &CodexLeaseBoundExpectation{
			Identity:         CodexJournalRecordIdentity{LaneDigest: "lane", TurnDigest: "turn", ModeEpoch: 7, Authoritative: true},
			AccountKey:       "account-a",
			RecordGeneration: 12,
		},
		claimed: true,
	}
	handler := newCodexRetainedHTTPTestHandler(t, planner)
	if err := handler.native.CloseAndDrain(context.Background()); err != nil {
		t.Fatal(err)
	}
	writer := httptest.NewRecorder()

	handled, model := handler.TryServe(writer, httptest.NewRequest(http.MethodPost, "http://localhost/v1/responses", bytes.NewReader(body)), false)

	if !handled || model != "" || writer.Code != http.StatusServiceUnavailable {
		t.Fatalf("closed retained claim = handled %t model %q status %d", handled, model, writer.Code)
	}
	if planner.probeCalls != 1 || planner.buildCalls != 0 {
		t.Fatalf("closed retained claim probe/build = %d/%d, want 1/0", planner.probeCalls, planner.buildCalls)
	}
}

func TestCodexRetainedNativeHTTPShutdownCancelsPreClaimPlanning(t *testing.T) {
	body := frozenRequestBody("gpt-5", CodexRequestTurn, "private-body")
	probeStarted := make(chan struct{})
	planner := &codexRetainedHTTPPlannerStub{probeStarted: probeStarted, probeWaitForCancellation: true}
	handler := newCodexRetainedHTTPTestHandler(t, planner)
	served := make(chan struct{})
	go func() {
		defer close(served)
		handler.TryServe(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "http://localhost/v1/responses", bytes.NewReader(body)), false)
	}()
	select {
	case <-probeStarted:
	case <-time.After(time.Second):
		t.Fatal("retained probe did not begin")
	}

	if err := handler.native.CloseAndDrain(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-served:
	case <-time.After(time.Second):
		t.Fatal("pre-claim retained planning did not observe shutdown")
	}
}

func TestCodexRetainedNativeHTTPSerialisesProbeThroughBegin(t *testing.T) {
	body := frozenRequestBody("gpt-5", CodexRequestTurn, "private-body")
	identity := CodexJournalRecordIdentity{LaneDigest: "lane", TurnDigest: "turn", ModeEpoch: 7, Authoritative: true}
	routes := &codexRetainedHTTPGenerationSnapshotter{snapshot: CodexLeaseRouteSnapshot{
		Classification:        CodexRestoredLaneCurrent,
		BoundAccountKey:       "account",
		BoundIdentity:         identity,
		BoundRecordGeneration: 12,
		BoundChoice:           RouteChoice{AccountKey: "account", EffectiveModel: "gpt-5"},
		JournalGeneration:     13,
	}}
	runtime := &codexRetainedHTTPPlanningRuntime{
		gates:  &codexLanePlanningGateSet{entries: make(map[LaneKey]*codexLanePlanningGateEntry)},
		routes: routes,
	}
	factory := codexHTTPRequestPlanTestFactory(runtime)
	factory.Routes = routes
	factory.Authority = CodexLeaseAuthorityPolicy{ModeEpoch: 9, RetainedAuthoritativeEpochs: []uint64{7}}
	native, err := NewCodexNativeHTTPHandler(factory, &codexNativeHTTPSessionStub{}, "https://codex.example")
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewCodexRetainedNativeHTTPHandler(factory, native)
	if err != nil {
		t.Fatal(err)
	}
	closeStarted := make(chan struct{})
	closeRelease := make(chan struct{})
	requestBody := &codexRetainedHTTPBlockingCloseBody{
		Reader:  bytes.NewReader(body),
		started: closeStarted,
		release: closeRelease,
	}
	writer := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		defer close(served)
		handler.TryServe(writer, httptest.NewRequest(http.MethodPost, "http://localhost/v1/responses", requestBody), false)
	}()
	select {
	case <-closeStarted:
	case <-time.After(time.Second):
		t.Fatal("retained probe did not reach claimed body close")
	}
	probeAcquires := runtime.acquireCount()

	type buildResult struct {
		prepared CodexPreparedHTTPRequest
		err      error
	}
	competing := make(chan buildResult, 1)
	go func() {
		prepared, buildErr := factory.Build(context.Background(), CodexHTTPRequestPlanInput{Encoded: bytes.Clone(body)})
		competing <- buildResult{prepared: prepared, err: buildErr}
	}()
	runtime.waitForAcquireCount(t, probeAcquires+1)
	var early *buildResult
	select {
	case result := <-competing:
		early = &result
	case <-time.After(30 * time.Millisecond):
	}
	close(closeRelease)
	select {
	case <-served:
	case <-time.After(time.Second):
		t.Fatal("retained request did not return")
	}
	var competitor buildResult
	if early != nil {
		competitor = *early
	} else {
		select {
		case competitor = <-competing:
		case <-time.After(time.Second):
			t.Fatal("competing request did not begin after retained request")
		}
	}
	if competitor.err == nil {
		if competitor.prepared.Lifecycle != nil {
			_, _ = competitor.prepared.Lifecycle.AbandonBeforeDispatchContext(context.Background())
		}
		if competitor.prepared.Frozen != nil {
			competitor.prepared.Frozen.Release()
		}
	}
	if early != nil {
		t.Fatal("same-lane build escaped retained probe planning guard")
	}
	if writer.Code != http.StatusBadGateway {
		t.Fatalf("retained request status = %d, want post-Begin session failure 502", writer.Code)
	}
	if runtime.beginCount() != 2 {
		t.Fatalf("begin calls = %d, want retained and competitor", runtime.beginCount())
	}
}

func TestCodexRetainedNativeHTTPProbeFailureClaimsWithoutBuild(t *testing.T) {
	t.Parallel()
	body := frozenRequestBody("gpt-5", CodexRequestTurn, "private-body")
	original := &codexRetainedHTTPTestBody{Reader: bytes.NewReader(body)}
	planner := &codexRetainedHTTPPlannerStub{claimed: true, probeErr: ErrCodexStaleTurn}
	handler := newCodexRetainedHTTPTestHandler(t, planner)
	writer := httptest.NewRecorder()

	handled, _ := handler.TryServe(writer, httptest.NewRequest(http.MethodPost, "http://localhost/v1/responses", original), false)
	if !handled || planner.probeCalls != 1 || planner.buildCalls != 0 || original.closes != 1 || writer.Code != http.StatusServiceUnavailable {
		t.Fatalf("probe failure = handled %t probe/build %d/%d closes %d status %d", handled, planner.probeCalls, planner.buildCalls, original.closes, writer.Code)
	}
}

func TestCodexRetainedNativeHTTPCancellationClosesBodyOnce(t *testing.T) {
	body := newCodexNativeHTTPBlockingBody()
	planner := &codexRetainedHTTPPlannerStub{}
	handler := newCodexRetainedHTTPTestHandler(t, planner)
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "http://localhost/v1/responses", body).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		handler.TryServe(httptest.NewRecorder(), request, false)
		close(done)
	}()
	<-body.started
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled retained probe remained blocked")
	}
	if body.closeCalls() != 1 || planner.probeCalls != 0 || planner.buildCalls != 0 {
		t.Fatalf("cancel = closes %d probe/build %d/%d", body.closeCalls(), planner.probeCalls, planner.buildCalls)
	}
}

func TestCodexRetainedNativeHTTPCancellationRecoversPrivateClosePanic(t *testing.T) {
	body := newCodexNativeHTTPPanickingCloseBody()
	planner := &codexRetainedHTTPPlannerStub{}
	handler := newCodexRetainedHTTPTestHandler(t, planner)
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "http://localhost/v1/responses", body).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		handler.TryServe(httptest.NewRecorder(), request, false)
		close(done)
	}()
	<-body.started
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("panicking retained body close escaped or remained blocked")
	}
	if planner.probeCalls != 0 || planner.buildCalls != 0 {
		t.Fatalf("private close panic reached probe/build %d/%d", planner.probeCalls, planner.buildCalls)
	}
}

func TestCodexRetainedNativeHTTPReadPanicFailsClosedPrivately(t *testing.T) {
	body := &codexRetainedHTTPPanickingReadBody{}
	planner := &codexRetainedHTTPPlannerStub{}
	handler := newCodexRetainedHTTPTestHandler(t, planner)
	writer := httptest.NewRecorder()

	handled, model := handler.TryServe(writer, httptest.NewRequest(http.MethodPost, "http://localhost/v1/responses", body), false)
	if !handled || model != "" || writer.Code != http.StatusBadRequest {
		t.Fatalf("read panic = handled %t model %q status %d", handled, model, writer.Code)
	}
	if planner.probeCalls != 0 || planner.buildCalls != 0 || body.closes != 1 {
		t.Fatalf("read panic reached probe/build %d/%d or closed %d times", planner.probeCalls, planner.buildCalls, body.closes)
	}
	if strings.Contains(writer.Body.String(), "private retained request read panic") {
		t.Fatalf("read panic disclosed private text: %s", writer.Body.String())
	}
}

type codexRetainedHTTPPlannerStub struct {
	expected                 *CodexLeaseBoundExpectation
	claimed                  bool
	probeErr                 error
	buildErr                 error
	probeCalls               int
	buildCalls               int
	buildExpected            *CodexLeaseBoundExpectation
	buildEncoded             []byte
	mutateProbe              bool
	probeStarted             chan struct{}
	probeWaitForCancellation bool
	buildStarted             chan struct{}
	buildRelease             <-chan struct{}
}

func (planner *codexRetainedHTTPPlannerStub) ProbeRetained(ctx context.Context, input CodexHTTPRequestPlanInput) (*CodexRetainedHTTPRequestClaim, bool, error) {
	planner.probeCalls++
	if planner.probeStarted != nil {
		close(planner.probeStarted)
	}
	if planner.probeWaitForCancellation {
		<-ctx.Done()
		return nil, true, ctx.Err()
	}
	if planner.mutateProbe {
		if len(input.Encoded) != 0 {
			input.Encoded[0] ^= 0xff
		}
		input.Headers.Set("X-Private", "mutated")
	}
	if planner.expected == nil {
		return nil, planner.claimed, planner.probeErr
	}
	return &CodexRetainedHTTPRequestClaim{ExpectedBound: *planner.expected}, planner.claimed, planner.probeErr
}

func (planner *codexRetainedHTTPPlannerStub) Build(_ context.Context, input CodexHTTPRequestPlanInput) (CodexPreparedHTTPRequest, error) {
	planner.buildCalls++
	if planner.buildStarted != nil {
		close(planner.buildStarted)
	}
	if planner.buildRelease != nil {
		<-planner.buildRelease
	}
	planner.buildEncoded = bytes.Clone(input.Encoded)
	if input.ExpectedBound != nil {
		expected := *input.ExpectedBound
		planner.buildExpected = &expected
	}
	return CodexPreparedHTTPRequest{}, planner.buildErr
}

type codexRetainedHTTPTestBody struct {
	*bytes.Reader
	closes int
}

type codexRetainedHTTPBlockingCloseBody struct {
	*bytes.Reader
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (body *codexRetainedHTTPBlockingCloseBody) Close() error {
	body.once.Do(func() { close(body.started) })
	<-body.release
	return nil
}

type codexRetainedHTTPGenerationSnapshotter struct {
	mu       sync.Mutex
	snapshot CodexLeaseRouteSnapshot
}

func (snapshotter *codexRetainedHTTPGenerationSnapshotter) LoadRouteSnapshot(context.Context, LeaseKey, []codex.AccountKey, CodexLeaseAuthorityPolicy) (CodexLeaseRouteSnapshot, error) {
	snapshotter.mu.Lock()
	defer snapshotter.mu.Unlock()
	return snapshotter.snapshot, nil
}

func (snapshotter *codexRetainedHTTPGenerationSnapshotter) advance() {
	snapshotter.mu.Lock()
	snapshotter.snapshot.BoundRecordGeneration++
	snapshotter.snapshot.JournalGeneration++
	snapshotter.mu.Unlock()
}

type codexRetainedHTTPPlanningRuntime struct {
	mu           sync.Mutex
	gates        *codexLanePlanningGateSet
	routes       *codexRetainedHTTPGenerationSnapshotter
	acquireCalls int
	beginCalls   int
}

func (runtime *codexRetainedHTTPPlanningRuntime) AcquireRequestPlanningContext(ctx context.Context, key LeaseKey, _ []codex.AccountKey, _ CodexLeaseAuthorityPolicy) (func(), error) {
	runtime.mu.Lock()
	runtime.acquireCalls++
	runtime.mu.Unlock()
	guard, err := runtime.gates.acquire(ctx, key.Lane)
	if err != nil {
		return nil, err
	}
	return guard.Release, nil
}

func (runtime *codexRetainedHTTPPlanningRuntime) BeginRequestContext(context.Context, CodexLeaseRequestPlan) (*CodexLeaseRequestHandle, error) {
	runtime.mu.Lock()
	runtime.beginCalls++
	runtime.mu.Unlock()
	runtime.routes.advance()
	return &CodexLeaseRequestHandle{account: "account"}, nil
}

func (runtime *codexRetainedHTTPPlanningRuntime) acquireCount() int {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.acquireCalls
}

func (runtime *codexRetainedHTTPPlanningRuntime) beginCount() int {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.beginCalls
}

func (runtime *codexRetainedHTTPPlanningRuntime) waitForAcquireCount(t *testing.T, want int) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for runtime.acquireCount() < want {
		select {
		case <-time.After(time.Millisecond):
		case <-deadline.C:
			t.Fatalf("planning acquire calls = %d, want at least %d", runtime.acquireCount(), want)
		}
	}
}

type codexRetainedHTTPPanickingReadBody struct {
	closes int
}

func (*codexRetainedHTTPPanickingReadBody) Read([]byte) (int, error) {
	panic("private retained request read panic")
}

func (body *codexRetainedHTTPPanickingReadBody) Close() error {
	body.closes++
	return nil
}

func (body *codexRetainedHTTPTestBody) Close() error {
	body.closes++
	return nil
}

func newCodexRetainedHTTPTestHandler(t *testing.T, planner *codexRetainedHTTPPlannerStub) *CodexRetainedNativeHTTPHandler {
	t.Helper()
	native, err := NewCodexNativeHTTPHandler(planner, &CodexHTTPRequestSession{}, "https://codex.example")
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewCodexRetainedNativeHTTPHandler(planner, native)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func assertCodexRetainedHTTPBody(t *testing.T, body io.ReadCloser, want []byte) {
	t.Helper()
	got, readErr := io.ReadAll(body)
	closeErr := body.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(got, want) {
		t.Fatalf("body = %d bytes readErr=%v closeErr=%v equal=%t", len(got), readErr, closeErr, bytes.Equal(got, want))
	}
}
