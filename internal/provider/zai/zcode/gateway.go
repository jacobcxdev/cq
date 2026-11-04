package zcode

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/provider/zai/zcode/messages"
)

const paidMessagesURL = "https://zcode.z.ai/api/v1/ultra-zai/anthropic/v1/messages"
const startMessagesURL = "https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages"
const maxGatewayRequest = 10 << 20

type GatewayAuth struct {
	Credential string `json:"-"`
	ZCodeJWT   string `json:"-"`
	DeviceMid  string `json:"-"`
	StartPlan  bool   `json:"start_plan"`
}

// Gateway sends one projected model request through the selected ZCode billing
// path. It never retries an inference request or forwards foreign client headers.
type Gateway struct {
	HTTP      httputil.Doer   `json:"-"`
	Signer    *Signer         `json:"-"`
	Version   string          `json:"version"`
	DeviceMid string          `json:"-"`
	Catalogue *ModelCatalogue `json:"-"`
}

func (g *Gateway) Do(ctx context.Context, body []byte, auth GatewayAuth, sessionID string) (out *http.Response, err error) {
	defer func() {
		if recover() != nil {
			out, err = nil, &Error{Code: "gateway_request_failed"}
		}
	}()
	if g == nil || g.HTTP == nil || !validHeaderValue(g.Version, 80) || g.DeviceMid != "" && !validHeaderValue(g.DeviceMid, 256) || auth.DeviceMid != "" && !validHeaderValue(auth.DeviceMid, 256) || !auth.StartPlan && (g.Signer == nil || g.Version != g.Signer.Version) {
		return nil, &Error{Code: "gateway_unavailable"}
	}
	if ctx.Err() != nil {
		return nil, &Error{Code: "gateway_cancelled"}
	}
	if len(body) > maxGatewayRequest {
		return nil, &Error{Code: "gateway_invalid_request"}
	}
	projected, codec, err := messages.Project(body)
	if err != nil || len(projected) > maxGatewayRequest {
		return nil, &Error{Code: "gateway_invalid_request"}
	}
	if g.Catalogue != nil {
		var request map[string]json.RawMessage
		_ = json.Unmarshal(projected, &request)
		var model string
		_ = json.Unmarshal(request["model"], &model)
		resolved, err := g.Catalogue.Resolve(ctx, model)
		if err != nil {
			return nil, err
		}
		if resolved != model {
			request["model"], _ = json.Marshal(resolved)
			projected, _ = json.Marshal(request)
		}
	}
	if sessionID != "" && !validHeaderValue(sessionID, 256) {
		return nil, &Error{Code: "gateway_invalid_request"}
	}
	sessionID = strings.TrimPrefix(strings.TrimPrefix(sessionID, "sess_"), "subagent_agent_")
	device := auth.DeviceMid
	if device == "" {
		device = g.DeviceMid
	}
	projected, err = gatewayMetadata(projected, device, sessionID)
	if err != nil || len(projected) > maxGatewayRequest {
		return nil, &Error{Code: "gateway_invalid_request"}
	}
	if auth.StartPlan && !validHeaderValue(auth.ZCodeJWT, 16<<10) || !auth.StartPlan && (!validHeaderValue(auth.Credential, 16<<10) || auth.ZCodeJWT != "" && !validHeaderValue(auth.ZCodeJWT, 16<<10)) {
		return nil, &Error{Code: "gateway_auth_unavailable"}
	}
	headers := make(http.Header)
	if !auth.StartPlan {
		headers, err = g.Signer.Headers(ctx, auth.Credential, sessionID)
		if err != nil {
			return nil, err
		}
	}
	endpoint := paidMessagesURL
	if auth.StartPlan {
		endpoint = startMessagesURL
	}
	requestCtx, cancel := context.WithCancel(ctx)
	retained := false
	defer func() {
		if !retained {
			cancel()
		}
	}()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(projected))
	if err != nil {
		return nil, &Error{Code: "gateway_invalid_request"}
	}
	req.Header = headers
	SetSourceHeaders(req.Header, g.Version)
	req.Header.Set("X-Request-Id", gatewayRequestID())
	req.Header.Set("X-ZCode-Trace-Id", gatewayRequestID())
	req.Header.Set("X-ZCode-Session-Type", "other")
	if sessionID != "" {
		req.Header.Set("X-Session-Id", sessionID)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Accept", "application/json")
	var mode struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(projected, &mode)
	if mode.Stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	if device != "" {
		req.Header.Set("X-Device-Mid", device)
	}
	if auth.StartPlan {
		req.Header.Set("Authorization", "Bearer "+auth.ZCodeJWT)
		req.Header.Set("X-Api-Key", auth.ZCodeJWT)
	} else {
		req.Header.Set("Authorization", "Bearer "+auth.Credential)
		req.Header.Set("X-Api-Key", auth.Credential)
		if auth.ZCodeJWT != "" {
			req.Header.Set("X-ZCode-Authorization", "Bearer "+auth.ZCodeJWT)
		}
	}
	doer := g.HTTP
	if client, ok := doer.(*http.Client); ok {
		private := *client
		private.Jar = nil
		private.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		doer = &private
	}
	if requestCtx.Err() != nil {
		return nil, &Error{Code: "gateway_cancelled"}
	}
	response, err := doer.Do(req)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = (&gatewaySource{ReadCloser: response.Body}).Close()
		}
		return nil, &Error{Code: "gateway_request_failed"}
	}
	if response == nil || response.Body == nil {
		return nil, &Error{Code: "gateway_invalid_response"}
	}
	source := &gatewaySource{ReadCloser: response.Body}
	defer func() {
		if !retained {
			_ = source.Close()
		}
	}()
	if response.StatusCode < 100 || response.StatusCode > 999 {
		return nil, &Error{Code: "gateway_invalid_response"}
	}
	result := *response
	result.Request, result.Trailer, result.TransferEncoding = nil, nil, nil
	result.Header = make(http.Header)
	for _, name := range []string{"Retry-After", "Cache-Control", "X-Request-Id"} {
		if value := response.Header.Get(name); value != "" {
			result.Header.Set(name, value)
		}
	}
	result.Uncompressed = true
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		setGatewayJSONBody(&result, []byte(`{"error":{"type":"api_error","message":"ZCode model request failed"}}`))
		return &result, nil
	}
	contentType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	switch contentType {
	case "application/json":
		data, err := httputil.ReadBody(source)
		if err != nil {
			return nil, &Error{Code: "gateway_read_failed"}
		}
		converted, err := codec.ConvertJSON(data)
		if err != nil {
			return nil, &Error{Code: "gateway_invalid_response"}
		}
		setGatewayJSONBody(&result, converted)
		return &result, nil
	case "text/event-stream":
		result.Header.Set("Content-Type", "text/event-stream")
		result.ContentLength = -1
		result.Body = gatewayStream(requestCtx, cancel, source, codec)
		retained = true
		return &result, nil
	default:
		return nil, &Error{Code: "gateway_invalid_content_type"}
	}
}

func gatewayRequestID() string {
	var value [16]byte
	_, _ = rand.Read(value[:])
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", value[:4], value[4:6], value[6:8], value[8:10], value[10:])
}

// The official Anthropic adapter attributes requests to the real device/session.
// This metadata is independent of paid request signing and applies to Start too.
func gatewayMetadata(body []byte, device, session string) ([]byte, error) {
	if device == "" {
		return body, nil
	}
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	identity, _ := json.Marshal(struct {
		Device  string `json:"device_id"`
		Account string `json:"account_uuid"`
		Session string `json:"session_id"`
	}{Device: device, Session: session})
	metadata, _ := json.Marshal(map[string]string{"user_id": string(identity)})
	request["metadata"] = metadata
	return json.Marshal(request)
}

func setGatewayJSONBody(response *http.Response, data []byte) {
	response.Header.Set("Content-Type", "application/json")
	response.Header.Set("Content-Length", strconv.Itoa(len(data)))
	response.ContentLength = int64(len(data))
	response.Body = io.NopCloser(bytes.NewReader(data))
}

type gatewaySource struct {
	io.ReadCloser
	once sync.Once
	err  error
}

func (s *gatewaySource) Close() error {
	s.once.Do(func() {
		defer func() {
			if recover() != nil {
				s.err = &Error{Code: "gateway_close_failed"}
			}
		}()
		if s.ReadCloser.Close() != nil {
			s.err = &Error{Code: "gateway_close_failed"}
		}
	})
	return s.err
}

type gatewayStreamBody struct {
	*io.PipeReader
	source *gatewaySource
	cancel context.CancelFunc
	stop   func() bool
}

func (b *gatewayStreamBody) Close() error {
	b.stop()
	b.cancel()
	_ = b.PipeReader.Close()
	return b.source.Close()
}

func gatewayStream(ctx context.Context, cancel context.CancelFunc, source *gatewaySource, codec *messages.Codec) io.ReadCloser {
	reader, writer := io.Pipe()
	stop := context.AfterFunc(ctx, func() {
		_ = source.Close()
		_ = writer.CloseWithError(&Error{Code: "gateway_cancelled"})
	})
	go func() {
		var conversionErr error
		defer func() {
			if recover() != nil {
				conversionErr = &Error{Code: "gateway_stream_failed"}
			}
			stop()
			_ = source.Close()
			_ = writer.CloseWithError(conversionErr)
			cancel()
		}()
		if codec.ConvertSSE(writer, source) != nil {
			conversionErr = &Error{Code: "gateway_stream_failed"}
		}
	}()
	return &gatewayStreamBody{PipeReader: reader, source: source, cancel: cancel, stop: stop}
}
