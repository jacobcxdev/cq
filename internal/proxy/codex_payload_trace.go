package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const codexPayloadCaptureMaxBytes = codexProtocolMaxBytes

func emitCodexTracePayload(ctx context.Context, event PayloadEvent) {
	trace := codexTraceFromContext(ctx)
	if trace == nil || trace.payload == nil {
		return
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	}
	event.EventType = "codex_payload"
	event.TraceID = trace.traceID
	event.ConnectionID = trace.connectionID
	event.Transport = trace.transport
	if event.SessionKey == "" {
		event.SessionKey = trace.sessionKey
		event.SessionSource = trace.sessionSource
	}
	if event.ThreadKey == "" {
		event.ThreadKey = trace.threadKey
	}
	if event.Body != nil && event.BodyBytes == 0 {
		event.BodyBytes = len(event.Body)
	}
	if err := trace.payload.Write(event); err != nil {
		fmt.Fprintf(os.Stderr, "%s cq: diagnostics: payload write trace_id=%s connection_id=%s: %v\n", time.Now().UTC().Format(time.RFC3339Nano), trace.traceID, trace.connectionID, err)
	}
}

func codexTraceHeaders(headers http.Header) http.Header {
	if headers == nil {
		return nil
	}
	result := make(http.Header)
	for name, values := range headers {
		switch strings.ToLower(name) {
		case "authorization", "proxy-authorization", "cookie", "set-cookie", "x-api-key", "x-cq-token":
			continue
		default:
			result[name] = append([]string(nil), values...)
		}
	}
	return result
}

func encodeCodexTracePayload(raw []byte) (json.RawMessage, string) {
	if json.Valid(raw) {
		return json.RawMessage(append([]byte(nil), raw...)), "json"
	}
	if utf8.Valid(raw) {
		encoded, _ := json.Marshal(string(raw))
		return encoded, "utf8"
	}
	encoded, _ := json.Marshal(base64.StdEncoding.EncodeToString(raw))
	return encoded, "base64"
}

type codexPayloadCaptureReadCloser struct {
	context   context.Context
	body      io.ReadCloser
	event     PayloadEvent
	buffer    bytes.Buffer
	bodyBytes int
	truncated bool
	once      sync.Once
	complete  bool
}

func captureCodexHTTPResponsePayload(ctx context.Context, response *http.Response) {
	captureCodexHTTPResponsePayloadWithEvent(ctx, response, PayloadEvent{
		Provider: "codex", RouteKind: "codex_native", Direction: "upstream_response",
	})
}

func captureCodexHTTPResponsePayloadWithEvent(ctx context.Context, response *http.Response, event PayloadEvent) {
	trace := codexTraceFromContext(ctx)
	if trace == nil || trace.payload == nil || response == nil || response.Body == nil {
		return
	}
	if _, wrapped := response.Body.(*codexPayloadCaptureReadCloser); wrapped {
		return
	}
	event.StatusCode = response.StatusCode
	event.Headers = codexTraceHeaders(response.Header)
	response.Body = &codexPayloadCaptureReadCloser{
		context: ctx,
		body:    response.Body,
		event:   event,
	}
}

func captureCodexHTTPAttemptPayload(ctx context.Context, request *http.Request, response *http.Response, choice RouteChoice, attempt CandidateAttempt) {
	event := PayloadEvent{
		Provider: "codex", RouteKind: "codex_native", Direction: "upstream_response",
		AccountHint: codexTraceAccountHint(choice.AccountKey), Attempt: attempt.Ordinal,
	}
	if request != nil {
		event.Method = request.Method
		if request.URL != nil {
			event.Path = request.URL.Path
			if strings.HasSuffix(event.Path, "/responses/compact") {
				event.RouteKind = "codex_compact"
			}
		}
		event.ClientKind = clientKindFromUserAgent(request.Header.Get("User-Agent"))
	}
	event.Model = choice.EffectiveModel
	if event.Model == "" {
		event.Model = choice.RequestedModel
	}
	captureCodexHTTPResponsePayloadWithEvent(ctx, response, event)
}

func emitCodexWSHandshakePayload(ctx context.Context, response *http.Response, body []byte, choice RouteChoice, attempt CandidateAttempt) {
	if response != nil && response.StatusCode == http.StatusSwitchingProtocols && len(body) == 0 {
		return
	}
	captured := body
	truncated := false
	if len(captured) > codexPayloadCaptureMaxBytes {
		captured = captured[:codexPayloadCaptureMaxBytes]
		truncated = true
	}
	encoded, encoding := encodeCodexTracePayload(captured)
	event := PayloadEvent{
		Method: http.MethodGet, Path: legacyCodexResponsesPath, Provider: "codex", RouteKind: "codex_websocket_handshake",
		Direction: "upstream_handshake_response", Model: choice.EffectiveModel,
		AccountHint: codexTraceAccountHint(choice.AccountKey), Attempt: attempt.Ordinal,
		Complete: true, Truncated: truncated, BodyBytes: len(body), BodyEncoding: encoding, Body: encoded,
	}
	if event.Model == "" {
		event.Model = choice.RequestedModel
	}
	if response != nil {
		event.StatusCode = response.StatusCode
		event.Headers = codexTraceHeaders(response.Header)
	}
	emitCodexTracePayload(ctx, event)
}

func (capture *codexPayloadCaptureReadCloser) Read(buffer []byte) (int, error) {
	n, err := capture.body.Read(buffer)
	if n > 0 {
		capture.bodyBytes += n
		remaining := codexPayloadCaptureMaxBytes - capture.buffer.Len()
		if remaining > 0 {
			written := min(n, remaining)
			_, _ = capture.buffer.Write(buffer[:written])
		}
		if n > remaining {
			capture.truncated = true
		}
	}
	if err == io.EOF {
		capture.complete = true
		capture.emit()
	}
	return n, err
}

func (capture *codexPayloadCaptureReadCloser) Close() error {
	err := capture.body.Close()
	capture.emit()
	return err
}

func (capture *codexPayloadCaptureReadCloser) emit() {
	capture.once.Do(func() {
		body := append([]byte(nil), capture.buffer.Bytes()...)
		capture.event.BodyBytes = capture.bodyBytes
		capture.event.Body, capture.event.BodyEncoding = encodeCodexTracePayload(body)
		capture.event.Complete = capture.complete
		capture.event.Truncated = capture.truncated
		emitCodexTracePayload(capture.context, capture.event)
		clearBytes(body)
	})
}
