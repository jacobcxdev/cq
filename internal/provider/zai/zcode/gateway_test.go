package zcode

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

const gatewayInput = `{"model":"glm-5.3","input":"hi","max_output_tokens":16}`
const gatewayReply = `{"id":"message-1","type":"message","role":"assistant","model":"glm-5.3","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":15,"output_tokens":3}}`

func gatewayFixture(doer doerFunc) *Gateway {
	return &Gateway{HTTP: doer, Version: "3.17.2", DeviceMid: "device-fixture", Signer: &Signer{Version: "3.17.2", HTTP: doerFunc(func(*http.Request) (*http.Response, error) { return signingResponse(signingFixtureCipher), nil })}}
}

func gatewayAuth(start bool) GatewayAuth {
	return GatewayAuth{Credential: "fixture-id.fixture-secret", ZCodeJWT: "zcode-fixture-token", StartPlan: start}
}

type countedBody struct {
	io.Reader
	closed atomic.Int32
}

func (b *countedBody) Close() error { b.closed.Add(1); return nil }

func TestGatewayJSONAuthenticationAndProjection(t *testing.T) {
	for _, start := range []bool{false, true} {
		calls := 0
		body := &countedBody{Reader: strings.NewReader(gatewayReply)}
		g := gatewayFixture(func(r *http.Request) (*http.Response, error) {
			calls++
			path, auth, associated := "/api/v1/ultra-zai/anthropic/v1/messages", "Bearer fixture-id.fixture-secret", "Bearer zcode-fixture-token"
			if start {
				path, auth, associated = "/api/v1/zcode-plan/anthropic/v1/messages", "Bearer zcode-fixture-token", ""
			}
			if r.Method != http.MethodPost || r.URL.Scheme != "https" || r.URL.Host != "zcode.z.ai" || r.URL.Path != path || r.URL.RawQuery != "" || r.Header.Get("Authorization") != auth || r.Header.Get("X-ZCode-Authorization") != associated {
				t.Fatal("gateway endpoint/authentication mismatch")
			}
			if r.Header.Get("X-Api-Key") != strings.TrimPrefix(auth, "Bearer ") {
				t.Fatal("Anthropic SDK API-key authentication missing")
			}
			if r.Header.Get("User-Agent") != "ZCode/3.17.2" || r.Header.Get("X-ZCode-App-Version") != "3.17.2" || r.Header.Get("X-Title") != "Z Code@electron" || r.Header.Get("HTTP-Referer") != "https://zcode.z.ai" || r.Header.Get("X-Device-Mid") != "device-fixture" || r.Header.Get("Anthropic-Version") != "2023-06-01" {
				t.Fatal("ZCode source identity missing")
			}
			for _, name := range []string{"Cookie", "ChatGPT-Account-Id", "OpenAI-Beta", "X-Client-Sign-Verified"} {
				if r.Header.Get(name) != "" {
					t.Fatal("foreign authentication reached ZCode")
				}
			}
			if start {
				if r.Header.Get("X-Client-Sig") != "" || r.Header.Get("X-Client-Pow") != "" {
					t.Fatal("Start request incorrectly used paid signing")
				}
			} else {
				public, _ := hex.DecodeString(signingFixturePublic)
				signed := r.Header.Clone()
				signed.Del("Authorization")
				verifySigningHeaders(t, signed, "fixture-id", "gateway-session", public)
			}
			var projected map[string]json.RawMessage
			data, _ := httputil.ReadBody(r.Body)
			if json.Unmarshal(data, &projected) != nil || projected["messages"] == nil || projected["input"] != nil || projected["max_tokens"] == nil {
				t.Fatal("request was not projected before network")
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json; charset=utf-8"}, "Content-Length": {"99999"}, "Set-Cookie": {"private"}}, Body: body}, nil
		})
		r, err := g.Do(context.Background(), []byte(gatewayInput), gatewayAuth(start), "gateway-session")
		if err != nil {
			t.Fatal(err)
		}
		data, err := httputil.ReadBody(r.Body)
		_ = r.Body.Close()
		var result struct{ Object, Status string }
		if err != nil || json.Unmarshal(data, &result) != nil || result.Object != "response" || result.Status != "completed" || r.Header.Get("Content-Type") != "application/json" || r.ContentLength != int64(len(data)) || r.Header.Get("Content-Length") != strconv.Itoa(len(data)) || r.Header.Get("Set-Cookie") != "" || r.Request != nil || calls != 1 || body.closed.Load() != 1 {
			t.Fatal("incorrect converted response or lifetime")
		}
	}
}

func TestGatewayStartDoesNotResolveOrSignPaidCredential(t *testing.T) {
	calls, handshakes := 0, 0
	for _, attachSigner := range []bool{false, true} {
		g := &Gateway{Version: "3.17.2", DeviceMid: "device-fixture", HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.URL.String() != startMessagesURL || r.Header.Get("Authorization") != "Bearer zcode-fixture-token" || r.Header.Get("X-Api-Key") != "zcode-fixture-token" {
				t.Fatal("Start request did not use ZCode JWT in both SDK auth headers")
			}
			for _, name := range []string{"X-Client-Sig", "X-Client-Pow", "X-Client-Nonce", "X-Client-Ts", "X-ZCode-Authorization"} {
				if r.Header.Get(name) != "" {
					t.Fatal("Start leaked paid authentication/signing headers")
				}
			}
			response := jsonResponse(gatewayReply)
			response.Header.Set("Content-Type", "application/json")
			return response, nil
		})}
		if attachSigner {
			g.Signer = &Signer{Version: "3.17.2", HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
				handshakes++
				t.Fatal("Start invoked paid signing handshake")
				return nil, nil
			})}
		}
		r, err := g.Do(context.Background(), []byte(gatewayInput), GatewayAuth{ZCodeJWT: "zcode-fixture-token", StartPlan: true}, "")
		if err != nil {
			t.Fatal("Start required a paid credential or signer")
		}
		_ = r.Body.Close()
	}
	if calls != 2 || handshakes != 0 {
		t.Fatal("unexpected Start request or signing count")
	}
}

func TestGatewayInvalidInputNeverReachesHandshake(t *testing.T) {
	for _, body := range []string{`{`, `{"model":"glm-5.3","input":"hi","previous_response_id":"opaque"}`, strings.Repeat("x", (10<<20)+1)} {
		g := gatewayFixture(func(*http.Request) (*http.Response, error) {
			t.Fatal("invalid input reached inference")
			return nil, nil
		})
		g.Signer.HTTP = doerFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("invalid input reached signing handshake")
			return nil, nil
		})
		if _, err := g.Do(context.Background(), []byte(body), gatewayAuth(false), "session"); err == nil {
			t.Fatal("invalid input accepted")
		}
	}
	encoded, _ := json.Marshal(gatewayAuth(false))
	if strings.Contains(string(encoded), "fixture") {
		t.Fatal("authentication JSON exposed credentials")
	}
}

func TestGatewayHTTPRejectionPreservedWithoutRetry(t *testing.T) {
	for _, status := range []int{301, 401, 403, 429, 500} {
		calls := 0
		body := &countedBody{Reader: strings.NewReader("fixture-id.fixture-secret zcode-fixture-token")}
		g := gatewayFixture(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": {"42"}, "Content-Type": {"text/plain"}}, Body: body}, nil
		})
		r, err := g.Do(context.Background(), []byte(gatewayInput), gatewayAuth(false), "session")
		if err != nil {
			t.Fatal(err)
		}
		data, err := httputil.ReadBody(r.Body)
		_ = r.Body.Close()
		if err != nil || r.StatusCode != status || r.Header.Get("Retry-After") != "42" || strings.Contains(string(data), "fixture") || calls != 1 || body.closed.Load() != 1 {
			t.Fatal("rejection lost status, exposed vendor data or retried inference")
		}
	}
}

func gatewaySSE() string {
	event := func(kind, data string) string { return "event: " + kind + "\ndata: " + data + "\n\n" }
	return event("message_start", `{"type":"message_start","message":{"id":"message-1","type":"message","role":"assistant","model":"glm-5.3","content":[],"usage":{"input_tokens":15,"output_tokens":0}}}`) +
		event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`) +
		event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"OK"}}`) +
		event("content_block_stop", `{"type":"content_block_stop","index":0}`) +
		event("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`) +
		event("message_stop", `{"type":"message_stop"}`)
}

func TestGatewaySSEConversionAndMetadata(t *testing.T) {
	body := &countedBody{Reader: strings.NewReader(gatewaySSE())}
	g := gatewayFixture(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream; charset=utf-8"}, "Content-Length": {"9999"}}, Body: body}, nil
	})
	r, err := g.Do(context.Background(), []byte(strings.TrimSuffix(gatewayInput, "}")+`,"stream":true}`), gatewayAuth(false), "session")
	if err != nil {
		t.Fatal(err)
	}
	data, err := httputil.ReadBody(r.Body)
	_ = r.Body.Close()
	if err != nil || !strings.Contains(string(data), "response.output_text.delta") || !strings.Contains(string(data), "response.completed") || r.ContentLength != -1 || r.Header.Get("Content-Length") != "" || r.Header.Get("Content-Type") != "text/event-stream" || body.closed.Load() != 1 {
		t.Fatal("stream conversion metadata or body lifetime incorrect")
	}
}

type blockingGatewayBody struct {
	started     chan struct{}
	closed      chan struct{}
	once, begin sync.Once
}

func (b *blockingGatewayBody) Read([]byte) (int, error) {
	b.begin.Do(func() { close(b.started) })
	<-b.closed
	return 0, errors.New("private-upstream-token")
}
func (b *blockingGatewayBody) Close() error { b.once.Do(func() { close(b.closed) }); return nil }

func TestGatewayStreamCancellationAndCloseReleaseOriginalBody(t *testing.T) {
	for _, cancelRequest := range []bool{false, true} {
		body := &blockingGatewayBody{started: make(chan struct{}), closed: make(chan struct{})}
		g := gatewayFixture(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body}, nil
		})
		ctx, cancel := context.WithCancel(context.Background())
		r, err := g.Do(ctx, []byte(gatewayInput), gatewayAuth(false), "session")
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		select {
		case <-body.started:
		case <-time.After(2 * time.Second):
			t.Fatal("conversion did not start")
		}
		if cancelRequest {
			cancel()
		} else {
			_ = r.Body.Close()
		}
		select {
		case <-body.closed:
		case <-time.After(2 * time.Second):
			t.Fatal("original body not closed")
		}
		data, err := httputil.ReadBody(r.Body)
		if err == nil || strings.Contains(string(data), "response.completed") || strings.Contains(err.Error(), "private-upstream-token") {
			t.Fatal("cancelled stream completed or exposed transport detail")
		}
		_ = r.Body.Close()
		cancel()
	}
}

type panicGatewayReader struct{}

func (panicGatewayReader) Read([]byte) (int, error) { panic("private-upstream-token") }

func TestGatewayTruncatedOrPanickingStreamNeverCompletes(t *testing.T) {
	for _, reader := range []io.Reader{strings.NewReader(""), strings.NewReader(strings.Replace(gatewaySSE(), "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", "", 1)), panicGatewayReader{}} {
		body := &countedBody{Reader: reader}
		g := gatewayFixture(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body}, nil
		})
		r, err := g.Do(context.Background(), []byte(gatewayInput), gatewayAuth(false), "session")
		if err != nil {
			t.Fatal(err)
		}
		data, err := httputil.ReadBody(r.Body)
		_ = r.Body.Close()
		if err == nil || strings.Contains(string(data), "response.completed") || strings.Contains(err.Error(), "private-upstream-token") || body.closed.Load() != 1 {
			t.Fatal("failed stream completed or retained original body")
		}
	}
}

func TestGatewayTransportAndJSONErrorsNeverRetry(t *testing.T) {
	for _, test := range []struct {
		body, contentType string
		transport         bool
	}{
		{transport: true},
		{body: strings.Repeat("x", httputil.MaxResponseBody+1), contentType: "application/json"},
		{body: `{"secret":"fixture-id.fixture-secret"}`, contentType: "application/json"},
		{body: gatewayReply, contentType: "text/plain"},
	} {
		calls := 0
		g := gatewayFixture(func(*http.Request) (*http.Response, error) {
			calls++
			if test.transport {
				return nil, errors.New("fixture-id.fixture-secret")
			}
			r := jsonResponse(test.body)
			r.Header.Set("Content-Type", test.contentType)
			return r, nil
		})
		if _, err := g.Do(context.Background(), []byte(gatewayInput), gatewayAuth(false), "session"); err == nil || strings.Contains(err.Error(), "fixture") || calls != 1 {
			t.Fatal("failed model request retried or exposed private details")
		}
	}
}

func TestGatewayNeverFollowsModelRedirect(t *testing.T) {
	calls := 0
	g := gatewayFixture(nil)
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls != 1 {
			t.Fatal("model redirect followed")
		}
		r := jsonResponse("")
		r.StatusCode = 307
		r.Header.Set("Location", "https://other.example/model")
		return r, nil
	})}
	g.HTTP = client
	r, err := g.Do(context.Background(), []byte(gatewayInput), gatewayAuth(false), "session")
	if err != nil || r.StatusCode != 307 || calls != 1 || client.CheckRedirect != nil {
		t.Fatal("redirect did not fail closed without mutating caller")
	}
}

func TestGatewayMissingAuthOrCancelledContextNeverSends(t *testing.T) {
	for _, auth := range []GatewayAuth{{}, {Credential: "fixture-id.fixture-secret", StartPlan: true}, {Credential: "fixture-id.fixture-secret", ZCodeJWT: "bad\nheader"}} {
		g := gatewayFixture(func(*http.Request) (*http.Response, error) { t.Fatal("invalid auth reached model"); return nil, nil })
		g.Signer.HTTP = doerFunc(func(*http.Request) (*http.Response, error) { t.Fatal("invalid auth reached signer"); return nil, nil })
		if _, err := g.Do(context.Background(), []byte(gatewayInput), auth, "session"); err == nil {
			t.Fatal("invalid authentication accepted")
		}
	}
	g := gatewayFixture(func(*http.Request) (*http.Response, error) {
		t.Fatal("cancelled request reached model")
		return nil, nil
	})
	g.Signer.HTTP = doerFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("cancelled request reached signer")
		return nil, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.Do(ctx, []byte(gatewayInput), gatewayAuth(false), "session"); err == nil {
		t.Fatal("cancelled request accepted")
	}
}

func TestGatewayInvalidStatusClosesBodyAndTransportPanicIsRedacted(t *testing.T) {
	body := &countedBody{Reader: strings.NewReader("")}
	g := gatewayFixture(func(*http.Request) (*http.Response, error) { return &http.Response{StatusCode: 0, Body: body}, nil })
	if _, err := g.Do(context.Background(), []byte(gatewayInput), gatewayAuth(false), "session"); err == nil || body.closed.Load() != 1 {
		t.Fatal("invalid response retained body")
	}
	g.HTTP = doerFunc(func(*http.Request) (*http.Response, error) { panic("fixture-id.fixture-secret") })
	if _, err := g.Do(context.Background(), []byte(gatewayInput), gatewayAuth(false), "session"); err == nil || strings.Contains(err.Error(), "fixture-secret") {
		t.Fatal("transport panic exposed credential")
	}
}
