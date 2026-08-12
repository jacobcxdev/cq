package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type codexRejectedTrackingBody struct {
	reader    io.Reader
	readBytes int
	closes    int
}

type codexRejectedBlockingBody struct {
	started   chan struct{}
	closed    chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
	closes    atomic.Int32
}

type codexRejectedFailingBody struct {
	value  []byte
	err    error
	sent   bool
	closes int
}

type codexRejectedGatedBody struct {
	reader    io.Reader
	started   chan struct{}
	allowRead chan struct{}
	startOnce sync.Once
	closes    atomic.Int32
}

type codexRejectedPrefixThenBlockingBody struct {
	prefix    *bytes.Reader
	started   chan struct{}
	closed    chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
	closes    atomic.Int32
}

type codexRejectedPanickingBlockingBody struct {
	started   chan struct{}
	closed    chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
	closes    atomic.Int32
}

func (body *codexRejectedPanickingBlockingBody) Read([]byte) (int, error) {
	body.startOnce.Do(func() { close(body.started) })
	<-body.closed
	return 0, errors.New("private read failure")
}

func (body *codexRejectedPanickingBlockingBody) Close() error {
	body.closes.Add(1)
	body.closeOnce.Do(func() {
		close(body.closed)
	})
	panic("Bearer private-close-token account@example.com")
}

func (body *codexRejectedPrefixThenBlockingBody) Read(p []byte) (int, error) {
	if n, _ := body.prefix.Read(p); n > 0 {
		return n, nil
	}
	body.startOnce.Do(func() { close(body.started) })
	<-body.closed
	return 0, errors.New("tail closed")
}

func (body *codexRejectedPrefixThenBlockingBody) Close() error {
	body.closes.Add(1)
	body.closeOnce.Do(func() {
		close(body.closed)
	})
	return nil
}

func (body *codexRejectedGatedBody) Read(p []byte) (int, error) {
	body.startOnce.Do(func() { close(body.started) })
	<-body.allowRead
	return body.reader.Read(p)
}

func (body *codexRejectedGatedBody) Close() error {
	body.closes.Add(1)
	return nil
}

func (body *codexRejectedFailingBody) Read(p []byte) (int, error) {
	if body.sent {
		return 0, body.err
	}
	body.sent = true
	return copy(p, body.value), body.err
}

func (body *codexRejectedFailingBody) Close() error {
	body.closes++
	return nil
}

func newCodexRejectedBlockingBody() *codexRejectedBlockingBody {
	return &codexRejectedBlockingBody{
		started: make(chan struct{}),
		closed:  make(chan struct{}),
	}
}

func (body *codexRejectedBlockingBody) Read([]byte) (int, error) {
	body.startOnce.Do(func() { close(body.started) })
	<-body.closed
	return 0, errors.New("private reader failure")
}

func (body *codexRejectedBlockingBody) Close() error {
	body.closes.Add(1)
	body.closeOnce.Do(func() {
		close(body.closed)
	})
	return nil
}

func (body *codexRejectedTrackingBody) Read(p []byte) (int, error) {
	n, err := body.reader.Read(p)
	body.readBytes += n
	return n, err
}

func (body *codexRejectedTrackingBody) Close() error {
	body.closes++
	return nil
}

func TestCodexRejectedResponseRetentionDiscardsOrdinaryResponse(t *testing.T) {
	body := &codexRejectedTrackingBody{reader: strings.NewReader("ordinary rejection")}
	response := &http.Response{StatusCode: http.StatusUnauthorized, Body: body}
	var retention CodexRejectedResponseRetention

	if err := retention.Reject(context.Background(), response, false); err != nil {
		t.Fatalf("Reject ordinary response: %v", err)
	}
	if body.readBytes != len("ordinary rejection") {
		t.Fatalf("read bytes = %d, want %d", body.readBytes, len("ordinary rejection"))
	}
	if body.closes != 1 {
		t.Fatalf("close count = %d, want 1", body.closes)
	}
	if _, err := retention.Exhausted(); !isCodexRejectedResponseError(err, CodexRejectedResponseNoRetainedDefault) {
		t.Fatalf("Exhausted error = %v", err)
	}
}

func TestCodexRejectedResponseRetentionTransfersExactLimitDefault(t *testing.T) {
	want := bytes.Repeat([]byte("x"), maxCodexRejectedResponseBytes)
	body := &codexRejectedTrackingBody{reader: bytes.NewReader(want)}
	sourceHeader := http.Header{"X-Test": {"frozen"}}
	response := &http.Response{
		Status:           "429 Too Many Requests",
		StatusCode:       http.StatusTooManyRequests,
		Proto:            "HTTP/2.0",
		ProtoMajor:       2,
		Header:           sourceHeader,
		Body:             body,
		ContentLength:    int64(len(want)),
		TransferEncoding: []string{"chunked", "private-adjacent"},
		Trailer:          http.Header{"X-Trailer": {"private-adjacent"}},
		Request:          &http.Request{Header: http.Header{"Authorization": {"Bearer private-token"}}},
		TLS:              &tls.ConnectionState{ServerName: "private.example"},
	}
	var retention CodexRejectedResponseRetention

	if err := retention.Reject(context.Background(), response, true); err != nil {
		t.Fatalf("Reject default response: %v", err)
	}
	if body.readBytes != len(want) || body.closes != 1 {
		t.Fatalf("source body read/close = %d/%d, want %d/1", body.readBytes, body.closes, len(want))
	}
	owned := retention.body
	if len(owned) != len(want) {
		t.Fatalf("retained bytes = %d, want %d", len(owned), len(want))
	}
	sourceHeader.Set("X-Test", "mutated")

	got, err := retention.Exhausted()
	if err != nil {
		t.Fatalf("Exhausted: %v", err)
	}
	if got == response {
		t.Fatal("Exhausted returned the caller's response object")
	}
	if got.Status != response.Status || got.StatusCode != response.StatusCode || got.Proto != response.Proto || got.ProtoMajor != response.ProtoMajor {
		t.Fatalf("retained status/proto = %#v", got)
	}
	if got.Header.Get("X-Test") != "frozen" {
		t.Fatalf("retained header = %q", got.Header.Get("X-Test"))
	}
	if got.Request != nil || got.TLS != nil || got.TransferEncoding != nil || got.Trailer != nil {
		t.Fatalf("auth-adjacent response state retained: %#v", got)
	}
	if got.ContentLength != 0 {
		t.Fatalf("ContentLength = %d, want zero-value metadata", got.ContentLength)
	}
	gotBody, readErr := io.ReadAll(got.Body)
	if readErr != nil {
		t.Fatalf("read retained body: %v", readErr)
	}
	if !bytes.Equal(gotBody, want) {
		t.Fatalf("retained body mismatch: got %d bytes", len(gotBody))
	}
	if allZero(owned) {
		t.Fatal("transferred body was wiped before Close")
	}
	if err := got.Body.Close(); err != nil {
		t.Fatalf("close retained body: %v", err)
	}
	if !allZero(owned) {
		t.Fatal("transferred body was not wiped on Close")
	}
	if _, err := retention.Exhausted(); !isCodexRejectedResponseError(err, CodexRejectedResponseReleased) {
		t.Fatalf("second Exhausted error = %v", err)
	}
}

func TestCodexRejectedResponseRetentionRejectsDuplicateDefault(t *testing.T) {
	firstBody := &codexRejectedTrackingBody{reader: strings.NewReader("first default")}
	secondBody := &codexRejectedTrackingBody{reader: strings.NewReader("second default")}
	var retention CodexRejectedResponseRetention
	if err := retention.Reject(context.Background(), &http.Response{
		StatusCode: http.StatusUnauthorized,
		Body:       firstBody,
	}, true); err != nil {
		t.Fatalf("Reject first default: %v", err)
	}
	firstOwned := retention.body

	err := retention.Reject(context.Background(), &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       secondBody,
	}, true)
	if !isCodexRejectedResponseError(err, CodexRejectedResponseDuplicateDefault) {
		t.Fatalf("duplicate Reject error = %v", err)
	}
	if secondBody.readBytes != len("second default") || secondBody.closes != 1 {
		t.Fatalf("duplicate body read/close = %d/%d", secondBody.readBytes, secondBody.closes)
	}
	if !bytes.Equal(retention.body, firstOwned) || allZero(firstOwned) {
		t.Fatal("duplicate response replaced or wiped the first default")
	}

	got, err := retention.Exhausted()
	if err != nil {
		t.Fatalf("Exhausted: %v", err)
	}
	if got.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want first default", got.StatusCode)
	}
	if body := readCodexRejectedBody(t, got.Body); body != "first default" {
		t.Fatalf("body = %q, want first default", body)
	}
}

func TestCodexRejectedResponseRetentionTransfersOversizeDefaultImmediately(t *testing.T) {
	want := bytes.Repeat([]byte("o"), maxCodexRejectedResponseBytes+257)
	sourceBody := &codexRejectedTrackingBody{reader: bytes.NewReader(want)}
	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       sourceBody,
	}
	var retention CodexRejectedResponseRetention

	err := retention.Reject(context.Background(), response, true)
	if !isCodexRejectedResponseError(err, CodexRejectedResponseBodyTooLarge) {
		t.Fatalf("oversize Reject error = %v", err)
	}
	if sourceBody.readBytes != maxCodexRejectedResponseBytes+1 {
		t.Fatalf("source bytes read = %d, want bounded prefix %d", sourceBody.readBytes, maxCodexRejectedResponseBytes+1)
	}
	if sourceBody.closes != 0 {
		t.Fatalf("source closed before transfer: %d", sourceBody.closes)
	}
	transferred, ok := response.Body.(*codexRejectedResponseBody)
	if !ok {
		t.Fatalf("response body type = %T, want owned-prefix transfer", response.Body)
	}
	ownedPrefix := transferred.body
	got, readErr := io.ReadAll(response.Body)
	if readErr != nil {
		t.Fatalf("read transferred body: %v", readErr)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("transferred body mismatch: got %d bytes", len(got))
	}
	if sourceBody.closes != 0 {
		t.Fatal("source closed before caller closed transferred body")
	}
	if err := response.Body.Close(); err != nil {
		t.Fatalf("close transferred body: %v", err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatalf("second close transferred body: %v", err)
	}
	if sourceBody.closes != 1 {
		t.Fatalf("source close count = %d, want 1", sourceBody.closes)
	}
	if !allZero(ownedPrefix) {
		t.Fatal("owned oversize prefix was not wiped on Close")
	}
	if _, err := retention.Exhausted(); !isCodexRejectedResponseError(err, CodexRejectedResponseReleased) {
		t.Fatalf("Exhausted after oversize error = %v", err)
	}

	laterBody := &codexRejectedTrackingBody{reader: strings.NewReader("later default")}
	err = retention.Reject(context.Background(), &http.Response{Body: laterBody}, true)
	if !isCodexRejectedResponseError(err, CodexRejectedResponseReleased) {
		t.Fatalf("later Reject error = %v", err)
	}
	if laterBody.readBytes != len("later default") || laterBody.closes != 1 {
		t.Fatalf("later body read/close = %d/%d", laterBody.readBytes, laterBody.closes)
	}
}

func TestCodexRejectedResponseRetentionPreservesDefaultAcrossOrdinaryDiscard(t *testing.T) {
	var retention CodexRejectedResponseRetention
	if err := retention.Reject(context.Background(), &http.Response{
		StatusCode: http.StatusUnauthorized,
		Body:       io.NopCloser(strings.NewReader("configured default")),
	}, true); err != nil {
		t.Fatalf("Reject default: %v", err)
	}
	ordinaryBody := &codexRejectedTrackingBody{reader: strings.NewReader("ordinary alternative")}
	if err := retention.Reject(context.Background(), &http.Response{
		StatusCode: http.StatusUnauthorized,
		Body:       ordinaryBody,
	}, false); err != nil {
		t.Fatalf("Reject ordinary: %v", err)
	}
	if ordinaryBody.readBytes != len("ordinary alternative") || ordinaryBody.closes != 1 {
		t.Fatalf("ordinary body read/close = %d/%d", ordinaryBody.readBytes, ordinaryBody.closes)
	}
	got, err := retention.Exhausted()
	if err != nil {
		t.Fatalf("Exhausted: %v", err)
	}
	if body := readCodexRejectedBody(t, got.Body); body != "configured default" {
		t.Fatalf("body = %q, want configured default", body)
	}
}

func TestCodexRejectedResponseRetentionPrefersLaterOutcome(t *testing.T) {
	tests := []struct {
		name          string
		laterResponse *http.Response
		laterError    error
	}{
		{
			name: "soft or server response",
			laterResponse: &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Body:       &codexRejectedTrackingBody{reader: strings.NewReader("later winner")},
			},
		},
		{
			name:       "network error",
			laterError: errors.New("network outcome"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var retention CodexRejectedResponseRetention
			if err := retention.Reject(context.Background(), &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Body:       io.NopCloser(strings.NewReader("retained default")),
			}, true); err != nil {
				t.Fatalf("Reject default: %v", err)
			}
			owned := retention.body

			gotResponse, gotError := retention.PreferLater(test.laterResponse, test.laterError)
			if gotResponse != test.laterResponse || gotError != test.laterError {
				t.Fatalf("PreferLater = (%p, %v), want exact (%p, %v)", gotResponse, gotError, test.laterResponse, test.laterError)
			}
			if !allZero(owned) {
				t.Fatal("retained default was not wiped")
			}
			if _, err := retention.Exhausted(); !isCodexRejectedResponseError(err, CodexRejectedResponseReleased) {
				t.Fatalf("Exhausted error = %v", err)
			}
			if test.laterResponse != nil {
				body := test.laterResponse.Body.(*codexRejectedTrackingBody)
				if body.readBytes != 0 || body.closes != 0 {
					t.Fatalf("later winner was consumed: %d/%d", body.readBytes, body.closes)
				}
				_ = test.laterResponse.Body.Close()
			}
		})
	}
}

func TestCodexRejectedResponseRetentionCancellationReleasesDefault(t *testing.T) {
	var retention CodexRejectedResponseRetention
	if err := retention.Reject(context.Background(), &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       io.NopCloser(strings.NewReader("private retained default")),
	}, true); err != nil {
		t.Fatalf("Reject default: %v", err)
	}
	owned := retention.body
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ordinaryBody := &codexRejectedTrackingBody{reader: strings.NewReader("ordinary")}

	err := retention.Reject(ctx, &http.Response{Body: ordinaryBody}, false)
	if !isCodexRejectedResponseError(err, CodexRejectedResponseCanceled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Reject error = %v", err)
	}
	if ordinaryBody.closes != 1 {
		t.Fatalf("ordinary close count = %d, want 1", ordinaryBody.closes)
	}
	if !allZero(owned) {
		t.Fatal("cancellation did not wipe retained default")
	}
	if _, err := retention.Exhausted(); !isCodexRejectedResponseError(err, CodexRejectedResponseReleased) {
		t.Fatalf("Exhausted error = %v", err)
	}
}

func TestCodexRejectedResponseRetentionCancellationInterruptsRead(t *testing.T) {
	body := newCodexRejectedBlockingBody()
	response := &http.Response{Body: body}
	ctx, cancel := context.WithCancel(context.Background())
	var retention CodexRejectedResponseRetention
	result := make(chan error, 1)
	go func() {
		result <- retention.Reject(ctx, response, true)
	}()
	<-body.started
	cancel()

	select {
	case err := <-result:
		if !isCodexRejectedResponseError(err, CodexRejectedResponseCanceled) || !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled Reject error = %v", err)
		}
	case <-time.After(time.Second):
		_ = body.Close()
		t.Fatal("cancellation did not interrupt response read")
	}
	if body.closes.Load() != 1 {
		t.Fatalf("close count = %d, want 1", body.closes.Load())
	}
	if _, err := retention.Exhausted(); !isCodexRejectedResponseError(err, CodexRejectedResponseReleased) {
		t.Fatalf("Exhausted error = %v", err)
	}
}

func TestCodexRejectedResponseRetentionCancellationRecoversClosePanic(t *testing.T) {
	body := &codexRejectedPanickingBlockingBody{
		started: make(chan struct{}),
		closed:  make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	var retention CodexRejectedResponseRetention
	result := make(chan error, 1)
	go func() {
		result <- retention.Reject(ctx, &http.Response{Body: body}, true)
	}()
	<-body.started
	cancel()

	select {
	case err := <-result:
		if !isCodexRejectedResponseError(err, CodexRejectedResponseCanceled) || !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled Reject error = %v", err)
		}
		if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "example.com") {
			t.Fatalf("close panic leaked through error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not complete after panicking Close")
	}
	if body.closes.Load() != 1 {
		t.Fatalf("close count = %d, want 1", body.closes.Load())
	}
}

func TestCodexRejectedResponseRetentionReleaseInterruptsReadAndIsIdempotent(t *testing.T) {
	body := newCodexRejectedBlockingBody()
	var retention CodexRejectedResponseRetention
	result := make(chan error, 1)
	go func() {
		result <- retention.Reject(context.Background(), &http.Response{Body: body}, true)
	}()
	<-body.started
	released := make(chan struct{})
	go func() {
		retention.Release()
		retention.Release()
		close(released)
	}()

	select {
	case <-released:
	case <-time.After(time.Second):
		_ = body.Close()
		t.Fatal("Release did not interrupt response read")
	}
	select {
	case err := <-result:
		if !isCodexRejectedResponseError(err, CodexRejectedResponseReleased) {
			t.Fatalf("Reject error after Release = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Reject did not return after Release")
	}
	if body.closes.Load() != 1 {
		t.Fatalf("close count = %d, want 1", body.closes.Load())
	}
	var nilRetention *CodexRejectedResponseRetention
	nilRetention.Release()
}

func TestCodexRejectedResponseRetentionCancellationWinsDuringReleasedInputDrain(t *testing.T) {
	var retention CodexRejectedResponseRetention
	retention.Release()
	body := newCodexRejectedBlockingBody()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- retention.Reject(ctx, &http.Response{Body: body}, true)
	}()
	<-body.started
	cancel()

	select {
	case err := <-result:
		if !isCodexRejectedResponseError(err, CodexRejectedResponseCanceled) || !errors.Is(err, context.Canceled) {
			t.Fatalf("Reject error = %v, want typed cancellation", err)
		}
	case <-time.After(time.Second):
		_ = body.Close()
		t.Fatal("cancellation did not interrupt released-input drain")
	}
	if body.closes.Load() != 1 {
		t.Fatalf("close count = %d, want 1", body.closes.Load())
	}
}

func TestCodexRejectedResponseOversizeCloseInterruptsTailRead(t *testing.T) {
	sourceBody := &codexRejectedPrefixThenBlockingBody{
		prefix:  bytes.NewReader(bytes.Repeat([]byte("o"), maxCodexRejectedResponseBytes+1)),
		started: make(chan struct{}),
		closed:  make(chan struct{}),
	}
	response := &http.Response{Body: sourceBody}
	var retention CodexRejectedResponseRetention
	if err := retention.Reject(context.Background(), response, true); !isCodexRejectedResponseError(err, CodexRejectedResponseBodyTooLarge) {
		t.Fatalf("oversize Reject error = %v", err)
	}
	transferred := response.Body.(*codexRejectedResponseBody)
	ownedPrefix := transferred.body
	readDone := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(response.Body)
		readDone <- err
	}()
	<-sourceBody.started
	closeDone := make(chan error, 1)
	go func() { closeDone <- response.Body.Close() }()

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("close transferred body: %v", err)
		}
	case <-time.After(time.Second):
		_ = sourceBody.Close()
		<-closeDone
		<-readDone
		t.Fatal("Close did not interrupt the original tail read")
	}
	if err := <-readDone; err == nil {
		t.Fatal("tail read unexpectedly completed without its close error")
	}
	if sourceBody.closes.Load() != 1 {
		t.Fatalf("source close count = %d, want 1", sourceBody.closes.Load())
	}
	if !allZero(ownedPrefix) {
		t.Fatal("owned oversize prefix was not wiped")
	}
}

func TestCodexRejectedResponseRetentionBoundsOrdinaryDrain(t *testing.T) {
	wantRead := maxCodexRejectedResponseBytes + 1
	body := &codexRejectedTrackingBody{reader: bytes.NewReader(bytes.Repeat([]byte("d"), wantRead+512))}
	var retention CodexRejectedResponseRetention
	if err := retention.Reject(context.Background(), &http.Response{Body: body}, false); err != nil {
		t.Fatalf("Reject ordinary response: %v", err)
	}
	if body.readBytes != wantRead {
		t.Fatalf("read bytes = %d, want bounded %d", body.readBytes, wantRead)
	}
	if body.closes != 1 {
		t.Fatalf("close count = %d, want 1", body.closes)
	}
}

func TestCodexRejectedResponseRetentionReleaseWipesOwnedState(t *testing.T) {
	var retention CodexRejectedResponseRetention
	if err := retention.Reject(context.Background(), &http.Response{
		Status:     "429 private-status",
		StatusCode: http.StatusTooManyRequests,
		Proto:      "HTTP/private",
		Header:     http.Header{"X-Private": {"private-token@example.com"}},
		Body:       io.NopCloser(strings.NewReader("private response body")),
	}, true); err != nil {
		t.Fatalf("Reject default: %v", err)
	}
	ownedBody := retention.body
	ownedResponse := retention.response

	retention.Release()
	retention.Release()
	if !allZero(ownedBody) {
		t.Fatal("Release did not wipe body bytes")
	}
	if ownedResponse.Status != "" || ownedResponse.Proto != "" || len(ownedResponse.Header) != 0 {
		t.Fatalf("Release did not clear response metadata: %#v", ownedResponse)
	}
	if _, err := retention.Exhausted(); !isCodexRejectedResponseError(err, CodexRejectedResponseReleased) {
		t.Fatalf("Exhausted error = %v", err)
	}

	laterBody := &codexRejectedTrackingBody{reader: strings.NewReader("later")}
	err := retention.Reject(context.Background(), &http.Response{Body: laterBody}, false)
	if !isCodexRejectedResponseError(err, CodexRejectedResponseReleased) {
		t.Fatalf("Reject after Release error = %v", err)
	}
	if laterBody.readBytes != len("later") || laterBody.closes != 1 {
		t.Fatalf("later body read/close = %d/%d", laterBody.readBytes, laterBody.closes)
	}
}

func TestCodexRejectedResponseRetentionErrorsAreTypedAndPrivate(t *testing.T) {
	private := "Bearer private-token account@example.com account-id-123"
	readerErr := errors.New(private)
	body := &codexRejectedFailingBody{value: []byte(private), err: readerErr}
	var retention CodexRejectedResponseRetention

	err := retention.Reject(context.Background(), &http.Response{Body: body}, true)
	if !isCodexRejectedResponseError(err, CodexRejectedResponseBodyReadFailed) {
		t.Fatalf("Reject error = %v", err)
	}
	if errors.Is(err, readerErr) {
		t.Fatal("private reader error was retained as an error cause")
	}
	if strings.Contains(err.Error(), private) || strings.Contains(err.Error(), "token") || strings.Contains(err.Error(), "example.com") {
		t.Fatalf("error leaked private data: %q", err)
	}
	if body.closes != 1 {
		t.Fatalf("close count = %d, want 1", body.closes)
	}
	encodedError, marshalErr := json.Marshal(err)
	if marshalErr != nil {
		t.Fatalf("marshal error: %v", marshalErr)
	}
	if strings.Contains(string(encodedError), private) {
		t.Fatalf("durable error leaked private data: %s", encodedError)
	}
	encodedOwner, marshalErr := json.Marshal(&retention)
	if marshalErr != nil {
		t.Fatalf("marshal retention: %v", marshalErr)
	}
	if string(encodedOwner) != "{}" {
		t.Fatalf("retention durable form = %s, want empty DTO", encodedOwner)
	}
}

func TestCodexRejectedResponseRetentionDoesNotRetainPrivateCancellationCause(t *testing.T) {
	privateCause := errors.New("Bearer private-token account@example.com account-id-123")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(privateCause)
	body := &codexRejectedTrackingBody{reader: strings.NewReader("discarded")}
	var retention CodexRejectedResponseRetention

	err := retention.Reject(ctx, &http.Response{Body: body}, false)
	if !isCodexRejectedResponseError(err, CodexRejectedResponseCanceled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("Reject error = %v, want safe typed cancellation", err)
	}
	if errors.Is(err, privateCause) || strings.Contains(err.Error(), privateCause.Error()) {
		t.Fatalf("cancellation error retained private cause: %v", err)
	}
	if body.closes != 1 {
		t.Fatalf("close count = %d, want 1", body.closes)
	}
}

func TestCodexRejectedResponseRetentionInvalidResponseReleasesDefault(t *testing.T) {
	var retention CodexRejectedResponseRetention
	if err := retention.Reject(context.Background(), &http.Response{
		Body: io.NopCloser(strings.NewReader("private retained default")),
	}, true); err != nil {
		t.Fatalf("Reject default: %v", err)
	}
	owned := retention.body

	err := retention.Reject(context.Background(), &http.Response{}, true)
	if !isCodexRejectedResponseError(err, CodexRejectedResponseInvalidResponse) {
		t.Fatalf("invalid Reject error = %v", err)
	}
	if !allZero(owned) {
		t.Fatal("invalid response did not wipe retained default")
	}
	if _, err := retention.Exhausted(); !isCodexRejectedResponseError(err, CodexRejectedResponseReleased) {
		t.Fatalf("Exhausted error = %v", err)
	}
}

func TestCodexRejectedResponseRetentionRejectsDuplicateWhileFirstIsReading(t *testing.T) {
	firstBody := &codexRejectedGatedBody{
		reader:    strings.NewReader("first default"),
		started:   make(chan struct{}),
		allowRead: make(chan struct{}),
	}
	var retention CodexRejectedResponseRetention
	firstResult := make(chan error, 1)
	go func() {
		firstResult <- retention.Reject(context.Background(), &http.Response{
			StatusCode: http.StatusUnauthorized,
			Body:       firstBody,
		}, true)
	}()
	<-firstBody.started

	secondBody := &codexRejectedTrackingBody{reader: strings.NewReader("second default")}
	err := retention.Reject(context.Background(), &http.Response{Body: secondBody}, true)
	if !isCodexRejectedResponseError(err, CodexRejectedResponseDuplicateDefault) {
		t.Fatalf("duplicate Reject error = %v", err)
	}
	if secondBody.readBytes != len("second default") || secondBody.closes != 1 {
		t.Fatalf("duplicate body read/close = %d/%d", secondBody.readBytes, secondBody.closes)
	}
	close(firstBody.allowRead)
	if err := <-firstResult; err != nil {
		t.Fatalf("first Reject: %v", err)
	}
	if firstBody.closes.Load() != 1 {
		t.Fatalf("first close count = %d, want 1", firstBody.closes.Load())
	}
	got, err := retention.Exhausted()
	if err != nil {
		t.Fatalf("Exhausted: %v", err)
	}
	if body := readCodexRejectedBody(t, got.Body); body != "first default" {
		t.Fatalf("body = %q, want first default", body)
	}
}

func TestCodexRejectedResponseRetentionPreferLaterInterruptsPendingDefault(t *testing.T) {
	body := newCodexRejectedBlockingBody()
	var retention CodexRejectedResponseRetention
	result := make(chan error, 1)
	go func() {
		result <- retention.Reject(context.Background(), &http.Response{Body: body}, true)
	}()
	<-body.started
	later := &http.Response{StatusCode: http.StatusBadGateway, Body: http.NoBody}
	laterErr := errors.New("later network outcome")

	gotResponse, gotError := retention.PreferLater(later, laterErr)
	if gotResponse != later || gotError != laterErr {
		t.Fatalf("PreferLater = (%p, %v), want exact (%p, %v)", gotResponse, gotError, later, laterErr)
	}
	select {
	case err := <-result:
		if !isCodexRejectedResponseError(err, CodexRejectedResponseReleased) {
			t.Fatalf("pending Reject error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("PreferLater did not release pending default")
	}
	if body.closes.Load() != 1 {
		t.Fatalf("close count = %d, want 1", body.closes.Load())
	}
}

func TestCodexRejectedResponseRetentionConcurrentTerminalOperations(t *testing.T) {
	var retention CodexRejectedResponseRetention
	if err := retention.Reject(context.Background(), &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       io.NopCloser(strings.NewReader("private retained default")),
	}, true); err != nil {
		t.Fatalf("Reject default: %v", err)
	}
	owned := retention.body

	const workers = 32
	errorsSeen := make(chan error, workers)
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		worker := worker
		wait.Add(1)
		go func() {
			defer wait.Done()
			switch worker % 4 {
			case 0:
				body := &codexRejectedTrackingBody{reader: strings.NewReader("ordinary")}
				err := retention.Reject(context.Background(), &http.Response{Body: body}, false)
				if err != nil && !isCodexRejectedResponseError(err, CodexRejectedResponseReleased) {
					errorsSeen <- err
				}
				if body.closes != 1 {
					errorsSeen <- errors.New("ordinary response was not closed")
				}
			case 1:
				response, err := retention.Exhausted()
				if err == nil {
					if closeErr := response.Body.Close(); closeErr != nil {
						errorsSeen <- closeErr
					}
				} else if !isCodexRejectedResponseError(err, CodexRejectedResponseReleased) {
					errorsSeen <- err
				}
			case 2:
				later := &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody}
				response, err := retention.PreferLater(later, nil)
				if err != nil || response != later {
					errorsSeen <- errors.New("later response was not passed through")
				}
			case 3:
				retention.Release()
			}
		}()
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Errorf("concurrent operation: %v", err)
	}
	if !allZero(owned) {
		t.Fatal("terminal operations left retained bytes unwiped")
	}
}

func isCodexRejectedResponseError(err error, code CodexRejectedResponseErrorCode) bool {
	var target *CodexRejectedResponseError
	return errors.As(err, &target) && target.Code == code
}

func readCodexRejectedBody(t *testing.T, body io.ReadCloser) string {
	t.Helper()
	defer body.Close()
	value, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return string(value)
}
