package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type degradedRescueTransportFailingLauncher struct {
	calls atomic.Int32
	err   error
}

func (launcher *degradedRescueTransportFailingLauncher) Launch(context.Context, WorkerManifestV1) (RuntimeWorkerProcess, error) {
	launcher.calls.Add(1)
	return nil, launcher.err
}

type degradedRescueProviderTransport struct {
	target *url.URL
	inner  *http.Transport
	calls  atomic.Int32
}

func (transport *degradedRescueProviderTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != "https" || request.URL.Host != "chatgpt.com" || !strings.HasPrefix(request.URL.EscapedPath(), "/backend-api/codex/") {
		return nil, fmt.Errorf("unexpected rescue origin %q", request.URL.String())
	}
	transport.calls.Add(1)
	outbound := request.Clone(request.Context())
	target := *request.URL
	target.Scheme = transport.target.Scheme
	target.Host = transport.target.Host
	outbound.URL = &target
	outbound.Host = transport.target.Host
	return transport.inner.RoundTrip(outbound)
}

type degradedRescueProviderReceipt struct {
	transport string
	method    string
	path      string
	authority string
	payload   string
	remote    string
}

type degradedRescueProviderBackend struct {
	receipts       chan degradedRescueProviderReceipt
	failures       chan error
	sseFlushed     chan struct{}
	sseRelease     chan struct{}
	sseReleaseOnce sync.Once
	upgrader       websocket.Upgrader
}

func newDegradedRescueProviderBackend() *degradedRescueProviderBackend {
	return &degradedRescueProviderBackend{
		receipts:   make(chan degradedRescueProviderReceipt, 4),
		failures:   make(chan error, 4),
		sseFlushed: make(chan struct{}),
		sseRelease: make(chan struct{}),
		upgrader: websocket.Upgrader{
			CheckOrigin:  func(*http.Request) bool { return true },
			Subprotocols: []string{"responses"},
		},
	}
}

func (backend *degradedRescueProviderBackend) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL.EscapedPath() != "/backend-api/codex/responses" {
		backend.fail(writer, http.StatusNotFound, fmt.Errorf("provider path = %q", request.URL.EscapedPath()))
		return
	}
	if request.Header.Get("Authorization") != "Bearer provider-token" {
		backend.fail(writer, http.StatusUnauthorized, fmt.Errorf("provider authorization = %q", request.Header.Get("Authorization")))
		return
	}
	if websocket.IsWebSocketUpgrade(request) {
		backend.serveWebSocket(writer, request)
		return
	}
	backend.serveSSE(writer, request)
}

func (backend *degradedRescueProviderBackend) serveSSE(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		backend.fail(writer, http.StatusMethodNotAllowed, fmt.Errorf("provider SSE method = %q", request.Method))
		return
	}
	payload, err := io.ReadAll(request.Body)
	if err != nil {
		backend.fail(writer, http.StatusBadRequest, err)
		return
	}
	backend.receipts <- degradedRescueProviderReceipt{
		transport: "http",
		method:    request.Method,
		path:      request.URL.EscapedPath(),
		authority: request.Header.Get("Authorization"),
		payload:   string(payload),
		remote:    request.RemoteAddr,
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("X-Mock-Provider-Transport", "network")
	_, _ = io.WriteString(writer, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"transport-http\"}}\n\n")
	flusher, ok := writer.(http.Flusher)
	if !ok {
		backend.failures <- errors.New("provider SSE response is not flushable")
		return
	}
	flusher.Flush()
	close(backend.sseFlushed)
	select {
	case <-backend.sseRelease:
	case <-request.Context().Done():
		backend.failures <- fmt.Errorf("provider SSE wait: %w", request.Context().Err())
		return
	}
	_, _ = io.WriteString(writer, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"transport-http\",\"end_turn\":true}}\n\n")
}

func (backend *degradedRescueProviderBackend) releaseSSE() {
	backend.sseReleaseOnce.Do(func() { close(backend.sseRelease) })
}

func (backend *degradedRescueProviderBackend) serveWebSocket(writer http.ResponseWriter, request *http.Request) {
	connection, err := backend.upgrader.Upgrade(writer, request, nil)
	if err != nil {
		backend.failures <- fmt.Errorf("provider WebSocket upgrade: %w", err)
		return
	}
	defer connection.Close()
	messageType, payload, err := connection.ReadMessage()
	if err != nil {
		backend.failures <- fmt.Errorf("provider WebSocket read: %w", err)
		return
	}
	if messageType != websocket.TextMessage {
		backend.failures <- fmt.Errorf("provider WebSocket message type = %d", messageType)
		return
	}
	backend.receipts <- degradedRescueProviderReceipt{
		transport: "websocket",
		method:    request.Method,
		path:      request.URL.EscapedPath(),
		authority: request.Header.Get("Authorization"),
		payload:   string(payload),
		remote:    request.RemoteAddr,
	}
	if err := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"transport-websocket","end_turn":true}}`)); err != nil {
		backend.failures <- fmt.Errorf("provider WebSocket write: %w", err)
	}
}

func (backend *degradedRescueProviderBackend) fail(writer http.ResponseWriter, status int, err error) {
	backend.failures <- err
	http.Error(writer, err.Error(), status)
}

func TestRuntimeSupervisorDegradedRescueRelaysHTTPAndWebSocketOverTransport(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	provider := newDegradedRescueProviderBackend()
	t.Cleanup(provider.releaseSSE)
	providerServer := httptest.NewServer(provider)
	t.Cleanup(providerServer.Close)
	providerURL, err := url.Parse(providerServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	innerTransport := http.DefaultTransport.(*http.Transport).Clone()
	innerTransport.Proxy = nil
	t.Cleanup(innerTransport.CloseIdleConnections)
	transport := &degradedRescueProviderTransport{target: providerURL, inner: innerTransport}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	forcedBootErr := errors.New("forced worker boot failure")
	launcher := &degradedRescueTransportFailingLauncher{err: forcedBootErr}
	events := []string{}
	supervisor, err := NewRuntimeSupervisor(
		listener,
		runtimeHolder("supervisor"),
		launcher,
		&runtimeTestCheckpointStore{events: &events},
	)
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	consumer := &callerAuthorityTestConsumer{consumed: make(map[string]ProviderBranchAdmissionConsumptionV1)}
	if err := supervisor.SetCallerAuthority(testNormalCallerAuthority(t, []NormalCallerCredentialV1{{
		Domain: NormalCallerLocal, Bearer: "control-token", SubjectID: "local-owner",
	}}, consumer)); err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	origin, err := url.Parse("https://chatgpt.com/backend-api/codex")
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	if err := supervisor.ConfigureRescue(ctx, &RescueRelay{Transport: transport, Origin: origin}, &runtimeEvidenceTestStore{}); err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	manifest := WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "transport-gate-worker"}
	if _, err := supervisor.Boot(ctx, manifest); !errors.Is(err, forcedBootErr) {
		_ = listener.Close()
		t.Fatalf("worker boot = %v, want forced failure", err)
	}
	if launcher.calls.Load() != 1 || supervisor.AdmissionReady() {
		_ = listener.Close()
		t.Fatalf("failed worker boot calls/ready = %d/%v, want 1/false", launcher.calls.Load(), supervisor.AdmissionReady())
	}

	proxyServer := &http.Server{Handler: supervisor}
	serveDone := make(chan error, 1)
	go func() { serveDone <- proxyServer.Serve(listener) }()
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer shutdownCancel()
		if err := proxyServer.Shutdown(shutdownCtx); err != nil {
			t.Errorf("shut down degraded rescue proxy: %v", err)
		}
		if err := <-serveDone; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve degraded rescue proxy: %v", err)
		}
	})
	baseURL := "http://" + listener.Addr().String()
	client := &http.Client{Timeout: 3 * time.Second}

	enter, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+RuntimeRescueEnterPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	enter.Header.Set("Authorization", "Bearer control-token")
	enterResponse, err := client.Do(enter)
	if err != nil {
		t.Fatal(err)
	}
	enterBody, readErr := io.ReadAll(enterResponse.Body)
	_ = enterResponse.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if enterResponse.StatusCode != http.StatusOK {
		t.Fatalf("rescue control = %d body=%q", enterResponse.StatusCode, enterBody)
	}
	var entered struct {
		Mode TrafficMode `json:"mode"`
	}
	if err := json.Unmarshal(enterBody, &entered); err != nil {
		t.Fatalf("decode rescue control response %q: %v", enterBody, err)
	}
	if entered.Mode != TrafficModeRescue {
		t.Fatalf("rescue control = %d mode=%q body=%q", enterResponse.StatusCode, entered.Mode, enterBody)
	}
	waitForRuntimeMode(t, supervisor, TrafficModeRescue)

	httpPayload := []byte(`{"model":"gpt-5.6-sol","stream":true}`)
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/responses", bytes.NewReader(httpPayload))
	if err != nil {
		t.Fatal(err)
	}
	httpRequest.Header.Set("Authorization", "Bearer provider-token")
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "text/event-stream")
	httpRequest.Header.Set("X-Codex-Window-Id", "degraded-http-window")
	httpResponse, err := client.Do(httpRequest)
	if err != nil {
		t.Fatal(err)
	}
	if httpResponse.StatusCode != http.StatusOK || httpResponse.Header.Get("Content-Type") != "text/event-stream" ||
		httpResponse.Header.Get("X-Mock-Provider-Transport") != "network" {
		_ = httpResponse.Body.Close()
		t.Fatalf("degraded rescue HTTP = %d content-type=%q transport=%q", httpResponse.StatusCode, httpResponse.Header.Get("Content-Type"), httpResponse.Header.Get("X-Mock-Provider-Transport"))
	}
	select {
	case <-provider.sseFlushed:
	case err := <-provider.failures:
		_ = httpResponse.Body.Close()
		t.Fatal(err)
	case <-ctx.Done():
		_ = httpResponse.Body.Close()
		t.Fatal(ctx.Err())
	}
	reader := bufio.NewReader(httpResponse.Body)
	createdEvent, err := reader.ReadString('\n')
	if err != nil {
		_ = httpResponse.Body.Close()
		t.Fatal(err)
	}
	createdData, err := reader.ReadString('\n')
	if err != nil {
		_ = httpResponse.Body.Close()
		t.Fatal(err)
	}
	createdEnd, err := reader.ReadString('\n')
	if err != nil {
		_ = httpResponse.Body.Close()
		t.Fatal(err)
	}
	if createdEvent != "event: response.created\n" || !strings.Contains(createdData, `"id":"transport-http"`) || createdEnd != "\n" {
		_ = httpResponse.Body.Close()
		t.Fatalf("degraded rescue first SSE event = %q%q%q", createdEvent, createdData, createdEnd)
	}
	provider.releaseSSE()
	httpTail, readErr := io.ReadAll(reader)
	closeErr := httpResponse.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read degraded rescue SSE completion: read=%v close=%v", readErr, closeErr)
	}
	if !bytes.Contains(httpTail, []byte(`"id":"transport-http","end_turn":true`)) {
		t.Fatalf("degraded rescue SSE completion = %q", httpTail)
	}

	webSocketURL := "ws://" + listener.Addr().String() + "/responses"
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second, Subprotocols: []string{"responses"}}
	webSocketHeader := http.Header{
		"Authorization":     []string{"Bearer provider-token"},
		"X-Codex-Window-Id": []string{"degraded-websocket-window"},
	}
	connection, response, err := dialer.DialContext(ctx, webSocketURL, webSocketHeader)
	if err != nil {
		if response != nil && response.Body != nil {
			body, _ := io.ReadAll(response.Body)
			_ = response.Body.Close()
			t.Fatalf("degraded rescue WebSocket dial = %v status=%d body=%q", err, response.StatusCode, body)
		}
		t.Fatal(err)
	}
	defer connection.Close()
	if connection.Subprotocol() != "responses" {
		t.Fatalf("degraded rescue WebSocket subprotocol = %q", connection.Subprotocol())
	}
	webSocketPayload := []byte(`{"type":"response.create","model":"gpt-5.6-sol"}`)
	if err := connection.WriteMessage(websocket.TextMessage, webSocketPayload); err != nil {
		t.Fatal(err)
	}
	messageType, webSocketResponse, err := connection.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.TextMessage || string(webSocketResponse) != `{"type":"response.completed","response":{"id":"transport-websocket","end_turn":true}}` {
		t.Fatalf("degraded rescue WebSocket response = type %d body %q", messageType, webSocketResponse)
	}
	_ = connection.Close()

	receipts := make(map[string]degradedRescueProviderReceipt, 2)
	for range 2 {
		select {
		case receipt := <-provider.receipts:
			receipts[receipt.transport] = receipt
		case err := <-provider.failures:
			t.Fatal(err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	wantPayloads := map[string]string{
		"http":      string(httpPayload),
		"websocket": string(webSocketPayload),
	}
	wantMethods := map[string]string{"http": http.MethodPost, "websocket": http.MethodGet}
	for transportName, wantPayload := range wantPayloads {
		receipt, ok := receipts[transportName]
		if !ok {
			t.Fatalf("missing provider %s receipt: %#v", transportName, receipts)
		}
		remoteHost, _, err := net.SplitHostPort(receipt.remote)
		if err != nil || !net.ParseIP(remoteHost).IsLoopback() {
			t.Fatalf("provider %s remote = %q, want loopback transport", transportName, receipt.remote)
		}
		if receipt.method != wantMethods[transportName] || receipt.path != "/backend-api/codex/responses" ||
			receipt.authority != "Bearer provider-token" || receipt.payload != wantPayload {
			t.Fatalf("provider %s receipt = %#v", transportName, receipt)
		}
	}
	if transport.calls.Load() != 2 {
		t.Fatalf("provider transport calls = %d, want HTTP and WebSocket", transport.calls.Load())
	}
	select {
	case err := <-provider.failures:
		t.Fatal(err)
	default:
	}
}
