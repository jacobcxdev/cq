package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestCodexNativeHTTPCloseAndDrainClosesAdmissionBeforeBodyRead(t *testing.T) {
	body := newCodexNativeHTTPBlockingBody()
	handler, err := NewCodexNativeHTTPHandler(
		&codexNativeHTTPPlannerStub{err: errors.New("stop after request read")},
		&CodexHTTPRequestSession{},
		"https://codex.example",
	)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, "http://localhost/v1/responses", body)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		handler.TryServe(httptest.NewRecorder(), request, false)
	}()
	select {
	case <-body.started:
	case <-time.After(time.Second):
		t.Fatal("native request did not begin reading")
	}

	drainContext, cancelDrain := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelDrain()
	if err := handler.CloseAndDrain(drainContext); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked drain error = %v, want deadline", err)
	}

	rejectedBody := &codexNativeHTTPUnreadBody{}
	rejectedRequest, err := http.NewRequest(http.MethodPost, "http://localhost/v1/responses", rejectedBody)
	if err != nil {
		t.Fatal(err)
	}
	rejectedWriter := httptest.NewRecorder()
	handled, _ := handler.TryServe(rejectedWriter, rejectedRequest, false)
	if !handled || rejectedWriter.Code != http.StatusServiceUnavailable {
		t.Fatalf("closed admission = handled %t status %d", handled, rejectedWriter.Code)
	}
	if reads, closes := rejectedBody.counts(); reads != 0 || closes != 1 {
		t.Fatalf("closed request body reads/closes = %d/%d, want 0/1", reads, closes)
	}

	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-served:
	case <-time.After(time.Second):
		t.Fatal("released native request did not return")
	}
	if err := handler.CloseAndDrain(context.Background()); err != nil {
		t.Fatalf("later drain error = %v", err)
	}
}

func TestCodexNativeHTTPCloseAndDrainCancelsBlockedPlanning(t *testing.T) {
	planner := &codexNativeHTTPBlockingPlanner{
		started: make(chan struct{}),
		stopped: make(chan struct{}),
	}
	handler, err := NewCodexNativeHTTPHandler(planner, &CodexHTTPRequestSession{}, "https://codex.example")
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, "http://localhost/v1/responses", bytes.NewReader(frozenRequestBody("gpt-5", CodexRequestTurn, "private")))
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		handler.TryServe(httptest.NewRecorder(), request, false)
	}()
	select {
	case <-planner.started:
	case <-time.After(time.Second):
		t.Fatal("native request did not begin planning")
	}

	drainContext, cancelDrain := context.WithTimeout(context.Background(), time.Second)
	defer cancelDrain()
	if err := handler.CloseAndDrain(drainContext); err != nil {
		t.Fatalf("drain blocked on cancelled planner: %v", err)
	}
	select {
	case <-planner.stopped:
	case <-time.After(time.Second):
		t.Fatal("planner did not observe handler shutdown")
	}
	select {
	case <-served:
	case <-time.After(time.Second):
		t.Fatal("cancelled native request did not return")
	}
}

func TestCodexNativeHTTPRequestPlanningContextCreatedAfterShutdownIsCancelled(t *testing.T) {
	handler, err := NewCodexNativeHTTPHandler(
		&codexNativeHTTPPlannerStub{},
		&CodexHTTPRequestSession{},
		"https://codex.example",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.CloseAndDrain(context.Background()); err != nil {
		t.Fatal(err)
	}

	planningContext, release := handler.requestPlanningContext(context.Background())
	defer release()
	if err := planningContext.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("planning context error = %v, want synchronous cancellation", err)
	}
}

func TestCodexNativeHTTPCloseAndDrainClosesAdmissionBeforeCancellingPlanning(t *testing.T) {
	handler, err := NewCodexNativeHTTPHandler(
		&codexNativeHTTPPlannerStub{err: errors.New("stop after request read")},
		&CodexHTTPRequestSession{},
		"https://codex.example",
	)
	if err != nil {
		t.Fatal(err)
	}
	rejectedBody := &codexNativeHTTPUnreadBody{}
	rejectedWriter := httptest.NewRecorder()
	originalCancel := handler.cancelPlanning
	handler.cancelPlanning = func() {
		request, requestErr := http.NewRequest(http.MethodPost, "http://localhost/v1/responses", rejectedBody)
		if requestErr != nil {
			t.Error(requestErr)
			return
		}
		handler.TryServe(rejectedWriter, request, false)
		originalCancel()
	}

	if err := handler.CloseAndDrain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rejectedWriter.Code != http.StatusServiceUnavailable {
		t.Fatalf("request during planning cancellation status = %d, want 503", rejectedWriter.Code)
	}
	if reads, closes := rejectedBody.counts(); reads != 0 || closes != 1 {
		t.Fatalf("request during planning cancellation reads/closes = %d/%d, want 0/1", reads, closes)
	}
}

func TestCodexNativeHTTPPanicReleasesExactlyOneAdmission(t *testing.T) {
	blockedBody := newCodexNativeHTTPBlockingBody()
	handler, err := NewCodexNativeHTTPHandler(
		codexNativeHTTPPanickingPlanner{},
		&CodexHTTPRequestSession{},
		"https://codex.example",
	)
	if err != nil {
		t.Fatal(err)
	}
	blockedRequest, err := http.NewRequest(http.MethodPost, "http://localhost/v1/responses", blockedBody)
	if err != nil {
		t.Fatal(err)
	}
	blockedServed := make(chan struct{})
	go func() {
		defer close(blockedServed)
		handler.TryServe(httptest.NewRecorder(), blockedRequest, false)
	}()
	select {
	case <-blockedBody.started:
	case <-time.After(time.Second):
		t.Fatal("blocking native request did not enter")
	}

	panicRequest, err := http.NewRequest(http.MethodPost, "http://localhost/v1/responses", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	panicRecovered := make(chan any, 1)
	go func() {
		defer func() { panicRecovered <- recover() }()
		handler.TryServe(httptest.NewRecorder(), panicRequest, false)
	}()
	select {
	case recovered := <-panicRecovered:
		if recovered == nil {
			t.Fatal("panicking planner did not panic")
		}
	case <-time.After(time.Second):
		t.Fatal("panicking native request did not return")
	}

	drainContext, cancelDrain := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelDrain()
	if err := handler.CloseAndDrain(drainContext); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain after one panic = %v, want still-active deadline", err)
	}
	if err := blockedBody.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-blockedServed:
	case <-time.After(time.Second):
		t.Fatal("remaining native request did not return")
	}
	if err := handler.CloseAndDrain(context.Background()); err != nil {
		t.Fatalf("final drain error = %v", err)
	}
}

type codexNativeHTTPUnreadBody struct {
	mu     sync.Mutex
	reads  int
	closes int
}

type codexNativeHTTPBlockingPlanner struct {
	started chan struct{}
	stopped chan struct{}
}

func (planner *codexNativeHTTPBlockingPlanner) Build(ctx context.Context, _ CodexHTTPRequestPlanInput) (CodexPreparedHTTPRequest, error) {
	close(planner.started)
	<-ctx.Done()
	close(planner.stopped)
	return CodexPreparedHTTPRequest{}, ctx.Err()
}

func (body *codexNativeHTTPUnreadBody) Read([]byte) (int, error) {
	body.mu.Lock()
	body.reads++
	body.mu.Unlock()
	return 0, io.EOF
}

func (body *codexNativeHTTPUnreadBody) Close() error {
	body.mu.Lock()
	body.closes++
	body.mu.Unlock()
	return nil
}

func (body *codexNativeHTTPUnreadBody) counts() (int, int) {
	body.mu.Lock()
	defer body.mu.Unlock()
	return body.reads, body.closes
}

type codexNativeHTTPPanickingPlanner struct{}

func (codexNativeHTTPPanickingPlanner) Build(context.Context, CodexHTTPRequestPlanInput) (CodexPreparedHTTPRequest, error) {
	panic("test native planner panic")
}
