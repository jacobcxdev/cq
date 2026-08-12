package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"sync"
	"testing"
	"time"
)

type codexEnvelopeRoundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip codexEnvelopeRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestCodexRequestEnvelopeOwnsImmutableCopies(t *testing.T) {
	encoded := []byte("encoded-request")
	decoded := []byte(`{"model":"gpt-5.4","input":"private"}`)
	headers := http.Header{
		"Content-Type":          {"application/json"},
		"Content-Encoding":      {"zstd"},
		"Openai-Beta":           {"responses=experimental"},
		"X-Codex-Turn-Metadata": {"opaque-metadata"},
	}
	envelope, err := NewCodexRequestEnvelope(encoded, decoded, headers, "gpt-5.4")
	if err != nil {
		t.Fatalf("NewCodexRequestEnvelope: %v", err)
	}
	defer envelope.Release()

	encoded[0] = 'X'
	decoded[0] = 'X'
	headers.Set("Content-Type", "text/plain")
	headers.Set("X-New", "late")

	first, err := envelope.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	defer first.Release()
	firstBody, err := first.Body()
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	if got := readCodexReplayBody(t, firstBody); got != "encoded-request" {
		t.Fatalf("body = %q, want exact frozen bytes", got)
	}
	firstDecoded, err := first.DecodedBody()
	if err != nil {
		t.Fatalf("DecodedBody: %v", err)
	}
	if got := string(firstDecoded); got != `{"model":"gpt-5.4","input":"private"}` {
		t.Fatalf("decoded body = %q", got)
	}
	firstHeader, err := first.Header()
	if err != nil {
		t.Fatalf("Header: %v", err)
	}
	if got := firstHeader.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := firstHeader.Get("X-New"); got != "" {
		t.Fatalf("late header retained: %q", got)
	}
	if got, err := first.EffectiveModel(); err != nil || got != "gpt-5.4" {
		t.Fatalf("effective model = %q", got)
	}
	if got, err := first.ContentLength(); err != nil || got != int64(len("encoded-request")) {
		t.Fatalf("content length = %d", got)
	}

	firstDecoded[0] = 'Y'
	firstHeader.Set("Content-Type", "application/mutated")
	second, err := envelope.Replay()
	if err != nil {
		t.Fatalf("second Replay: %v", err)
	}
	defer second.Release()
	secondDecoded, err := second.DecodedBody()
	if err != nil {
		t.Fatalf("second DecodedBody: %v", err)
	}
	if got := string(secondDecoded); got != `{"model":"gpt-5.4","input":"private"}` {
		t.Fatalf("second decoded body = %q", got)
	}
	secondHeader, err := second.Header()
	if err != nil {
		t.Fatalf("second Header: %v", err)
	}
	if got := secondHeader.Get("Content-Type"); got != "application/json" {
		t.Fatalf("second Content-Type = %q", got)
	}
}

func TestCodexRequestEnvelopeReplaysExactIndependentBodies(t *testing.T) {
	want := []byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, 0xff, 0x7f, 0x01}
	envelope, err := NewCodexRequestEnvelope(want, []byte("decoded"), nil, "gpt-5.4")
	if err != nil {
		t.Fatalf("NewCodexRequestEnvelope: %v", err)
	}
	defer envelope.Release()

	replay, err := envelope.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	defer replay.Release()
	replayBody, err := replay.Body()
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	defer replayBody.Close()
	body, err := io.ReadAll(replayBody)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(body, want) {
		t.Fatalf("body = %x, want %x", body, want)
	}

	for i := 0; i < 2; i++ {
		bodyCopy, err := replay.GetBody()
		if err != nil {
			t.Fatalf("GetBody %d: %v", i, err)
		}
		got, err := io.ReadAll(bodyCopy)
		closeErr := bodyCopy.Close()
		if err != nil {
			t.Fatalf("read GetBody %d: %v", i, err)
		}
		if closeErr != nil {
			t.Fatalf("close GetBody %d: %v", i, closeErr)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("GetBody %d = %x, want %x", i, got, want)
		}
	}
}

func TestCodexRequestReplayReleasePreservesAsyncTransportBodyUntilClose(t *testing.T) {
	want := bytes.Repeat([]byte("async-request-body-"), 128)
	envelope, err := NewCodexRequestEnvelope(want, []byte("decoded"), nil, "gpt-5.4")
	if err != nil {
		t.Fatalf("NewCodexRequestEnvelope: %v", err)
	}
	replay, err := envelope.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	body, err := replay.Body()
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	request, err := http.NewRequest(http.MethodPost, "https://example.test/responses", body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	type result struct {
		body     []byte
		readErr  error
		closeErr error
	}
	started := make(chan struct{})
	continueRead := make(chan struct{})
	finished := make(chan result, 1)
	transport := codexEnvelopeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		go func() {
			close(started)
			<-continueRead
			got, readErr := io.ReadAll(request.Body)
			finished <- result{body: got, readErr: readErr, closeErr: request.Body.Close()}
		}()
		return &http.Response{
			StatusCode: http.StatusAccepted,
			Body:       http.NoBody,
			Request:    request,
		}, nil
	})

	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer response.Body.Close()
	<-started

	state := replay.state
	ownedEncoded := state.encoded
	replay.Release()
	envelope.Release()
	if _, err := replay.GetBody(); !errors.Is(err, ErrCodexRequestEnvelopeReleased) {
		t.Fatalf("GetBody after release error = %v", err)
	}
	if allZero(ownedEncoded) {
		t.Fatal("active transport body was wiped before Close")
	}

	close(continueRead)
	got := <-finished
	if got.readErr != nil || got.closeErr != nil {
		t.Fatalf("async body read/close = %v/%v", got.readErr, got.closeErr)
	}
	if !bytes.Equal(got.body, want) {
		t.Fatalf("async body = %q, want exact replay", got.body)
	}
	if !allZero(ownedEncoded) {
		t.Fatal("replay body was not wiped after final Close")
	}
}

func TestCodexRequestReplayConcurrentCloseWaitsForFinalCleanup(t *testing.T) {
	envelope, err := NewCodexRequestEnvelope([]byte("encoded-request"), []byte("decoded-request"), nil, "gpt-5.4")
	if err != nil {
		t.Fatalf("NewCodexRequestEnvelope: %v", err)
	}
	bodyReplay, err := envelope.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	body, err := bodyReplay.Body()
	if err != nil {
		t.Fatalf("Body: %v", err)
	}

	state := bodyReplay.state
	ownedEncoded := state.encoded
	bodyReplay.Release()
	envelope.Release()
	state.mu.Lock()

	start := make(chan struct{})
	done := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for range 2 {
		go func() {
			ready.Done()
			<-start
			done <- body.Close()
		}()
	}
	ready.Wait()
	close(start)

	returnedBeforeCleanup := false
	select {
	case closeErr := <-done:
		returnedBeforeCleanup = true
		if closeErr != nil {
			t.Errorf("early Close: %v", closeErr)
		}
	case <-time.After(50 * time.Millisecond):
	}
	state.mu.Unlock()

	remaining := 2
	if returnedBeforeCleanup {
		remaining--
	}
	for range remaining {
		if closeErr := <-done; closeErr != nil {
			t.Errorf("Close: %v", closeErr)
		}
	}
	if returnedBeforeCleanup {
		t.Fatal("concurrent Close returned before final replay cleanup completed")
	}
	if !allZero(ownedEncoded) {
		t.Fatal("replay body was not wiped after concurrent Close completed")
	}
}

func TestCodexRequestEnvelopeRetainsOnlySafeSemanticHeaders(t *testing.T) {
	headers := http.Header{
		"authorization":                          {"Bearer secret"},
		"Proxy-Authorization":                    {"Basic secret"},
		"Proxy-Authenticate":                     {"Basic realm=secret"},
		"x-api-key":                              {"caller-secret"},
		"chatgpt-account-id":                     {"caller-account"},
		"cOoKiE":                                 {"session=secret"},
		"Cookie2":                                {"session2=secret"},
		"cOnNeCtIoN":                             {"keep-alive, X-Routing-Secret", "x-another-hop"},
		"X-Routing-Secret":                       {"secret"},
		"x-another-hop":                          {"also-secret"},
		"Keep-Alive":                             {"timeout=5"},
		"Proxy-Connection":                       {"keep-alive"},
		"Te":                                     {"trailers"},
		"Trailer":                                {"X-Trailer"},
		"Transfer-Encoding":                      {"chunked"},
		"Upgrade":                                {"websocket"},
		"Content-Length":                         {"999999"},
		"Author\u0130zation":                     {"unicode-confusable"},
		"X-Invalid-\u00dcnicode":                 {"invalid-name"},
		"X-Bad\x00Name":                          {"invalid-control"},
		"content-type":                           {"application/json"},
		"Content-Encoding":                       {"zstd"},
		"Accept":                                 {"text/event-stream"},
		"Accept-Encoding":                        {"gzip, zstd"},
		"User-Agent":                             {"codex-cli/0.146.0"},
		"Openai-Alpha":                           {"alpha-feature"},
		"Openai-Beta":                            {"first"},
		"openai-beta":                            {"second"},
		"X-Codex-Turn-Metadata":                  {"turn-metadata"},
		"X-Codex-Turn-State":                     {"turn-state"},
		"X-Codex-Installation-Id":                {"installation-id"},
		"X-Codex-Parent-Thread-Id":               {"parent-thread-id"},
		"X-Codex-Window-Id":                      {"window-id"},
		"X-Openai-Subagent":                      {"subagent"},
		"X-Openai-Memgen-Request":                {"memgen"},
		"X-Openai-Internal-Codex-Responses-Lite": {"responses-lite"},
		"X-Responsesapi-Include-Timing-Metrics":  {"true"},
	}
	envelope, err := NewCodexRequestEnvelope([]byte("body"), []byte("body"), headers, "gpt-5.4")
	if err != nil {
		t.Fatalf("NewCodexRequestEnvelope: %v", err)
	}
	defer envelope.Release()

	replay, err := envelope.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	defer replay.Release()
	replayHeader, err := replay.Header()
	if err != nil {
		t.Fatalf("Header: %v", err)
	}
	for _, name := range []string{
		"Authorization",
		"Proxy-Authorization",
		"Proxy-Authenticate",
		"X-Api-Key",
		"Chatgpt-Account-Id",
		"Cookie",
		"Cookie2",
		"Connection",
		"X-Routing-Secret",
		"X-Another-Hop",
		"Keep-Alive",
		"Proxy-Connection",
		"Te",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
		"Content-Length",
		"Author\u0130zation",
		"X-Invalid-\u00dcnicode",
		"X-Bad\x00Name",
	} {
		if values, ok := replayHeader[name]; ok {
			t.Errorf("unsafe header %q retained: %q", name, values)
		}
	}
	if got := replayHeader.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	for name, want := range map[string]string{
		"Content-Encoding":                       "zstd",
		"Accept":                                 "text/event-stream",
		"Accept-Encoding":                        "gzip, zstd",
		"User-Agent":                             "codex-cli/0.146.0",
		"Openai-Alpha":                           "alpha-feature",
		"X-Codex-Turn-Metadata":                  "turn-metadata",
		"X-Codex-Turn-State":                     "turn-state",
		"X-Codex-Installation-Id":                "installation-id",
		"X-Codex-Parent-Thread-Id":               "parent-thread-id",
		"X-Codex-Window-Id":                      "window-id",
		"X-Openai-Subagent":                      "subagent",
		"X-Openai-Memgen-Request":                "memgen",
		"X-Openai-Internal-Codex-Responses-Lite": "responses-lite",
		"X-Responsesapi-Include-Timing-Metrics":  "true",
	} {
		if got := replayHeader.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if got := replayHeader.Values("Openai-Beta"); len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("Openai-Beta = %q", got)
	}
}

func TestCodexRequestEnvelopeDropsConnectionNominatedSemanticHeader(t *testing.T) {
	headers := http.Header{
		"Connection":            {"keep-alive, x-codex-window-id"},
		"X-Codex-Window-Id":     {"must-not-survive"},
		"X-Codex-Turn-Metadata": {"must-survive"},
	}
	envelope, err := NewCodexRequestEnvelope([]byte("body"), []byte("body"), headers, "gpt-5.4")
	if err != nil {
		t.Fatalf("NewCodexRequestEnvelope: %v", err)
	}
	defer envelope.Release()
	replay, err := envelope.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	defer replay.Release()
	got, err := replay.Header()
	if err != nil {
		t.Fatalf("Header: %v", err)
	}
	if value := got.Get("X-Codex-Window-Id"); value != "" {
		t.Fatalf("Connection-nominated header retained: %q", value)
	}
	if value := got.Get("X-Codex-Turn-Metadata"); value != "must-survive" {
		t.Fatalf("semantic header = %q", value)
	}
}

func TestCodexRequestEnvelopeDropsUnknownHeaders(t *testing.T) {
	headers := http.Header{
		"Content-Type":        {"application/json"},
		"X-Custom-Secret":     {"unknown-secret"},
		"X-Oai-Attestation":   {"attestation-secret"},
		"Openai-Organization": {"organisation-secret"},
		"Openai-Project":      {"project-secret"},
		"Traceparent":         {"trace-secret"},
		"X-Openai-Future":     {"future-openai-secret"},
		"X-Codex-Future":      {"future-codex-secret"},
		"X-Openai-Session-Id": {"session-secret"},
		"X-Codex-Trace-Id":    {"codex-trace-secret"},
	}
	envelope, err := NewCodexRequestEnvelope([]byte("body"), []byte("body"), headers, "gpt-5.4")
	if err != nil {
		t.Fatalf("NewCodexRequestEnvelope: %v", err)
	}
	defer envelope.Release()
	replay, err := envelope.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	defer replay.Release()
	got, err := replay.Header()
	if err != nil {
		t.Fatalf("Header: %v", err)
	}
	if value := got.Get("Content-Type"); value != "application/json" {
		t.Fatalf("Content-Type = %q", value)
	}
	for _, name := range []string{
		"X-Custom-Secret",
		"X-Oai-Attestation",
		"Openai-Organization",
		"Openai-Project",
		"Traceparent",
		"X-Openai-Future",
		"X-Codex-Future",
		"X-Openai-Session-Id",
		"X-Codex-Trace-Id",
	} {
		if value := got.Get(name); value != "" {
			t.Errorf("unknown header %q retained: %q", name, value)
		}
	}
}

func TestCodexRequestEnvelopeAcceptsBodyOverLegacyLimit(t *testing.T) {
	body := codexProtocolRequestBodyAtSize(t, maxRequestBody+1)
	envelope, err := NewCodexRequestEnvelope(body, body, nil, "gpt-5")
	if err != nil {
		t.Fatalf("body over legacy limit rejected: %v", err)
	}
	defer envelope.Release()
	replay, err := envelope.Replay()
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Release()
	got, err := replay.DecodedBody()
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("decoded replay = %d bytes, %v; want %d bytes", len(got), err, len(body))
	}
}

func TestCodexRequestEnvelopeReleaseClearsOwnedState(t *testing.T) {
	headers := http.Header{"Content-Type": {"application/json"}}
	envelope, err := NewCodexRequestEnvelope([]byte("encoded"), []byte("decoded"), headers, "gpt-5.4")
	if err != nil {
		t.Fatalf("NewCodexRequestEnvelope: %v", err)
	}
	ownedEncoded := envelope.encoded
	ownedDecoded := envelope.decoded
	ownedHeaders := envelope.headers

	envelope.Release()
	envelope.Release()
	var nilEnvelope *CodexRequestEnvelope
	nilEnvelope.Release()

	if !allZero(ownedEncoded) {
		t.Fatalf("owned encoded bytes not overwritten: %q", ownedEncoded)
	}
	if !allZero(ownedDecoded) {
		t.Fatalf("owned decoded bytes not overwritten: %q", ownedDecoded)
	}
	if len(ownedHeaders) != 0 {
		t.Fatalf("owned headers not cleared: %q", ownedHeaders)
	}
	if envelope.encoded != nil || envelope.decoded != nil || envelope.headers != nil || envelope.effectiveModel != "" {
		t.Fatalf("released envelope retains references: %#v", envelope)
	}
	if got := headers.Get("Content-Type"); got != "application/json" {
		t.Fatalf("release mutated caller headers: %q", got)
	}
	if _, err := envelope.Replay(); !errors.Is(err, ErrCodexRequestEnvelopeReleased) {
		t.Fatalf("post-release Replay error = %v", err)
	}
}

func TestCodexRequestEnvelopeReleaseLetsExistingBodyFinish(t *testing.T) {
	envelope, err := NewCodexRequestEnvelope(
		[]byte("encoded-request"),
		[]byte("decoded-request"),
		http.Header{"Content-Type": {"application/json"}},
		"gpt-5.4",
	)
	if err != nil {
		t.Fatalf("NewCodexRequestEnvelope: %v", err)
	}
	replay, err := envelope.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}

	body, err := replay.Body()
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	ownedEncoded := replay.state.encoded
	envelope.Release()

	if contents, err := io.ReadAll(body); err != nil || string(contents) != "encoded-request" {
		t.Fatalf("existing body after release = %q, err %v", contents, err)
	}
	if allZero(ownedEncoded) {
		t.Fatal("existing body was wiped before Close")
	}
	if err := body.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}
	if !allZero(ownedEncoded) {
		t.Fatal("existing body was not wiped after Close")
	}
	if nextBody, err := replay.GetBody(); !errors.Is(err, ErrCodexRequestEnvelopeReleased) || nextBody != nil {
		t.Fatalf("GetBody after release = %#v, err %v", nextBody, err)
	}
	if decoded, err := replay.DecodedBody(); !errors.Is(err, ErrCodexRequestEnvelopeReleased) || decoded != nil {
		t.Fatalf("DecodedBody after release = %q, err %v", decoded, err)
	}
	if header, err := replay.Header(); !errors.Is(err, ErrCodexRequestEnvelopeReleased) || header != nil {
		t.Fatalf("Header after release = %q, err %v", header, err)
	}
}

func TestCodexRequestReplayReleaseClearsOwnedState(t *testing.T) {
	envelope, err := NewCodexRequestEnvelope(
		[]byte("encoded-request"),
		[]byte("decoded-request"),
		http.Header{"Content-Type": {"application/json"}},
		"gpt-5.4",
	)
	if err != nil {
		t.Fatalf("NewCodexRequestEnvelope: %v", err)
	}
	defer envelope.Release()

	replay, err := envelope.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	otherReplay, err := envelope.Replay()
	if err != nil {
		t.Fatalf("other Replay: %v", err)
	}
	defer otherReplay.Release()
	body, err := replay.Body()
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	getBody, err := replay.GetBody()
	if err != nil {
		t.Fatalf("GetBody: %v", err)
	}
	state := replay.state
	ownedEncoded := state.encoded
	ownedDecoded := state.decoded
	ownedHeaders := state.headers

	replay.Release()
	replay.Release()
	var nilReplay *CodexRequestReplay
	nilReplay.Release()

	if allZero(ownedEncoded) || allZero(ownedDecoded) || len(ownedHeaders) == 0 {
		t.Fatal("active replay state was cleared before its bodies closed")
	}
	if contents, readErr := io.ReadAll(body); readErr != nil || string(contents) != "encoded-request" {
		t.Fatalf("Body after release = %q, err %v", contents, readErr)
	}
	if closeErr := body.Close(); closeErr != nil {
		t.Fatalf("close Body: %v", closeErr)
	}
	if allZero(ownedEncoded) {
		t.Fatal("replay was cleared while GetBody remained open")
	}
	if contents, readErr := io.ReadAll(getBody); readErr != nil || string(contents) != "encoded-request" {
		t.Fatalf("GetBody after release = %q, err %v", contents, readErr)
	}
	if closeErr := getBody.Close(); closeErr != nil {
		t.Fatalf("close GetBody: %v", closeErr)
	}
	if !allZero(ownedEncoded) {
		t.Fatalf("replay encoded bytes not overwritten: %q", ownedEncoded)
	}
	if !allZero(ownedDecoded) {
		t.Fatalf("replay decoded bytes not overwritten: %q", ownedDecoded)
	}
	if len(ownedHeaders) != 0 {
		t.Fatalf("replay headers not cleared: %q", ownedHeaders)
	}
	if state.encoded != nil || state.decoded != nil || state.headers != nil || state.effectiveModel != "" || !state.released {
		t.Fatalf("released replay retains owned state: %#v", state)
	}
	if nextBody, bodyErr := replay.Body(); !errors.Is(bodyErr, ErrCodexRequestEnvelopeReleased) || nextBody != nil {
		t.Errorf("Body after release = %#v, err %v", nextBody, bodyErr)
	}
	if decoded, decodedErr := replay.DecodedBody(); !errors.Is(decodedErr, ErrCodexRequestEnvelopeReleased) || decoded != nil {
		t.Errorf("DecodedBody after release = %q, err %v", decoded, decodedErr)
	}
	if header, headerErr := replay.Header(); !errors.Is(headerErr, ErrCodexRequestEnvelopeReleased) || header != nil {
		t.Errorf("Header after release = %q, err %v", header, headerErr)
	}
	if model, modelErr := replay.EffectiveModel(); !errors.Is(modelErr, ErrCodexRequestEnvelopeReleased) || model != "" {
		t.Errorf("EffectiveModel after release = %q, err %v", model, modelErr)
	}
	if length, lengthErr := replay.ContentLength(); !errors.Is(lengthErr, ErrCodexRequestEnvelopeReleased) || length != 0 {
		t.Errorf("ContentLength after release = %d, err %v", length, lengthErr)
	}

	otherBody, err := otherReplay.Body()
	if err != nil {
		t.Fatalf("other Body: %v", err)
	}
	if got := readCodexReplayBody(t, otherBody); got != "encoded-request" {
		t.Fatalf("other replay body = %q", got)
	}
}

func TestCodexRequestReplayRemainsRegisteredUntilInvalidated(t *testing.T) {
	envelope, err := NewCodexRequestEnvelope([]byte("encoded"), []byte("decoded"), nil, "gpt-5.4")
	if err != nil {
		t.Fatalf("NewCodexRequestEnvelope: %v", err)
	}
	replay, err := envelope.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	state := replay.state
	state.mu.RLock()
	releaseDone := make(chan struct{})
	go func() {
		replay.Release()
		close(releaseDone)
	}()

	unregistered := false
	for attempt := 0; attempt < 1_000; attempt++ {
		envelope.mu.Lock()
		_, registered := envelope.replays[state]
		envelope.mu.Unlock()
		if !registered {
			unregistered = true
			break
		}
		runtime.Gosched()
	}
	if unregistered {
		envelope.Release()
	}
	state.mu.RUnlock()
	<-releaseDone
	envelope.Release()

	if unregistered {
		t.Fatal("replay was unregistered before its owned state was invalidated")
	}
}

func TestCodexRequestReplayBodiesRaceRelease(t *testing.T) {
	encoded := bytes.Repeat([]byte("sensitive-body-"), 256)
	envelope, err := NewCodexRequestEnvelope(encoded, []byte("decoded-request"), nil, "gpt-5.4")
	if err != nil {
		t.Fatalf("NewCodexRequestEnvelope: %v", err)
	}
	defer envelope.Release()
	replay, err := envelope.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	state := replay.state
	ownedEncoded := state.encoded
	ownedDecoded := state.decoded

	const readers = 8
	bodies := make([]io.ReadCloser, 0, readers)
	for reader := 0; reader < readers; reader++ {
		var body io.ReadCloser
		if reader%2 == 0 {
			body, err = replay.Body()
		} else {
			body, err = replay.GetBody()
		}
		if err != nil {
			t.Fatalf("body %d: %v", reader, err)
		}
		prefix := make([]byte, 1)
		if n, readErr := body.Read(prefix); n != 1 || readErr != nil || prefix[0] != encoded[0] {
			t.Fatalf("body %d prefix = %q, n %d, err %v", reader, prefix, n, readErr)
		}
		bodies = append(bodies, body)
	}

	start := make(chan struct{})
	failures := make(chan error, readers)
	var wait sync.WaitGroup
	for index, body := range bodies {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			var got []byte
			buffer := make([]byte, 1)
			for {
				n, readErr := body.Read(buffer)
				got = append(got, buffer[:n]...)
				if readErr == nil {
					runtime.Gosched()
					continue
				}
				if !errors.Is(readErr, io.EOF) {
					failures <- fmt.Errorf("body %d: %w", index, readErr)
					return
				}
				if !bytes.Equal(got, encoded[1:1+len(got)]) {
					failures <- fmt.Errorf("body %d changed bytes: %q", index, got)
				}
				return
			}
		}()
	}
	wait.Add(1)
	go func() {
		defer wait.Done()
		<-start
		replay.Release()
	}()
	close(start)
	wait.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
	if allZero(ownedEncoded) || allZero(ownedDecoded) {
		t.Fatal("raced replay state was cleared before its bodies closed")
	}
	for index, body := range bodies {
		if closeErr := body.Close(); closeErr != nil {
			t.Errorf("close body %d: %v", index, closeErr)
		}
		if n, readErr := body.Read(make([]byte, 1)); n != 0 || !errors.Is(readErr, http.ErrBodyReadAfterClose) {
			t.Errorf("closed body %d read = %d, err %v", index, n, readErr)
		}
	}
	if !allZero(ownedEncoded) {
		t.Fatal("raced encoded bytes not overwritten after final Close")
	}
	if !allZero(ownedDecoded) {
		t.Fatal("raced decoded bytes not overwritten after final Close")
	}
}

func TestCodexRequestReplayReturnedCopiesHaveCallerLifetime(t *testing.T) {
	envelope, err := NewCodexRequestEnvelope(
		[]byte("encoded-request"),
		[]byte("decoded-request"),
		http.Header{"Content-Type": {"application/json"}},
		"gpt-5.4",
	)
	if err != nil {
		t.Fatalf("NewCodexRequestEnvelope: %v", err)
	}
	replay, err := envelope.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	decoded, err := replay.DecodedBody()
	if err != nil {
		t.Fatalf("DecodedBody: %v", err)
	}
	header, err := replay.Header()
	if err != nil {
		t.Fatalf("Header: %v", err)
	}

	envelope.Release()

	if got := string(decoded); got != "decoded-request" {
		t.Fatalf("caller-owned decoded copy = %q", got)
	}
	if got := header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("caller-owned header copy = %q", got)
	}
	decoded[0] = 'X'
	header.Set("Content-Type", "caller-owned")
}

func TestCodexRequestEnvelopeConcurrentReplayAndRelease(t *testing.T) {
	const workers = 12
	envelope, err := NewCodexRequestEnvelope(
		[]byte("encoded-request"),
		[]byte("decoded-request"),
		http.Header{"Content-Type": {"application/json"}},
		"gpt-5.4",
	)
	if err != nil {
		t.Fatalf("NewCodexRequestEnvelope: %v", err)
	}

	start := make(chan struct{})
	failures := make(chan error, 1)
	report := func(err error) {
		select {
		case failures <- err:
		default:
		}
	}
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			for attempt := 0; attempt < 100; attempt++ {
				replay, err := envelope.Replay()
				if errors.Is(err, ErrCodexRequestEnvelopeReleased) {
					continue
				}
				if err != nil {
					report(fmt.Errorf("Replay: %w", err))
					return
				}
				bodyReader, err := replay.Body()
				if errors.Is(err, ErrCodexRequestEnvelopeReleased) {
					replay.Release()
					continue
				}
				if err != nil {
					report(fmt.Errorf("Body: %w", err))
					replay.Release()
					return
				}
				body, readErr := io.ReadAll(bodyReader)
				closeErr := bodyReader.Close()
				if readErr != nil || closeErr != nil || string(body) != "encoded-request" {
					report(fmt.Errorf("body=%q read=%v close=%v", body, readErr, closeErr))
					replay.Release()
					return
				}
				decoded, decodedErr := replay.DecodedBody()
				header, headerErr := replay.Header()
				model, modelErr := replay.EffectiveModel()
				if errors.Is(decodedErr, ErrCodexRequestEnvelopeReleased) || errors.Is(headerErr, ErrCodexRequestEnvelopeReleased) || errors.Is(modelErr, ErrCodexRequestEnvelopeReleased) {
					replay.Release()
					continue
				}
				if decodedErr != nil || headerErr != nil || modelErr != nil || string(decoded) != "decoded-request" || header.Get("Content-Type") != "application/json" || model != "gpt-5.4" {
					report(fmt.Errorf("partial replay: decoded=%q headers=%q model=%q errors=%v/%v/%v", decoded, header, model, decodedErr, headerErr, modelErr))
					replay.Release()
					return
				}
				decoded[0] = 'X'
				header.Set("Content-Type", "mutated")
				replay.Release()
			}
		}()
	}
	wait.Add(1)
	go func() {
		defer wait.Done()
		<-start
		for i := 0; i < 100; i++ {
			envelope.Release()
		}
	}()
	close(start)
	wait.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	if _, err := envelope.Replay(); !errors.Is(err, ErrCodexRequestEnvelopeReleased) {
		t.Fatalf("final Replay error = %v", err)
	}
}

func readCodexReplayBody(t *testing.T, body io.ReadCloser) string {
	t.Helper()
	defer body.Close()
	contents, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read replay body: %v", err)
	}
	return string(contents)
}

func allZero(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}
