package dots

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/harness"
)

type cloudGatewayFixture struct {
	gateway      *cloudHarnessGateway
	harness      *cloudHarness
	address      string
	peers        chan *websocket.Conn
	nextSequence uint64
}

func newCloudGatewayFixture(t *testing.T) *cloudGatewayFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h := &cloudHarness{b: &cloudBridge{ctx: ctx}, threads: make(map[string]*cloudHarnessThread), requests: make(map[string]cloudHarnessRequest), calls: make(map[string]chan cloudMessage), prefix: "cq-harness/fixture/", sequence: 100, wake: make(chan struct{}, 1)}
	f := &cloudGatewayFixture{harness: h, peers: make(chan *websocket.Conn, 8)}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RequestURI() != "/backend-api/wham/remote/control/server" || r.Header.Get("Authorization") != "Bearer fixture-token" || r.Header.Get("X-Codex-Server-Id") != "fixture-server" || r.Header.Get("X-Codex-Subscribe-Cursor") != "fixture-cursor" {
			t.Error("gateway changed upstream endpoint or authentication/upgrade metadata")
		}
		connection, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, http.Header{"X-Fixture-Upgrade": []string{"retained"}})
		if err == nil {
			f.peers <- connection
		}
	}))
	f.gateway = newCloudHarnessGateway(h)
	f.gateway.origin = upstream.URL
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.address = "ws://" + listener.Addr().String() + "/backend-api/wham/remote/control/server"
	done := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				done <- errors.New("gateway fixture panicked")
			}
		}()
		done <- f.gateway.Serve(listener)
	}()
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("gateway server shutdown: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("gateway did not stop with bridge context")
		}
		upstream.Close()
	})
	return f
}

func (f *cloudGatewayFixture) connect(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()
	native, response, err := websocket.DefaultDialer.Dial(f.address, http.Header{"Authorization": []string{"Bearer fixture-token"}, "X-Codex-Server-Id": []string{"fixture-server"}, "X-Codex-Subscribe-Cursor": []string{"fixture-cursor"}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Header.Get("X-Fixture-Upgrade") != "retained" {
		t.Fatal("upstream upgrade metadata was lost")
	}
	var backend *websocket.Conn
	select {
	case backend = <-f.peers:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream gateway connection unavailable")
	}
	t.Cleanup(func() { _ = native.Close(); _ = backend.Close() })
	return native, backend
}

func cloudGatewayRead(t *testing.T, connection *websocket.Conn) []byte {
	t.Helper()
	_ = connection.SetReadDeadline(time.Now().Add(3 * time.Second))
	kind, raw, err := connection.ReadMessage()
	if err != nil || kind != websocket.TextMessage {
		t.Fatalf("gateway frame unavailable: kind=%d err=%v", kind, err)
	}
	return raw
}

func cloudGatewaySend(t *testing.T, connection *websocket.Conn, raw []byte) {
	t.Helper()
	if err := connection.WriteMessage(websocket.TextMessage, raw); err != nil {
		t.Fatal(err)
	}
}

func (f *cloudGatewayFixture) subscribe(t *testing.T, native, backend *websocket.Conn, thread string) string {
	t.Helper()
	request := cloudHarnessRaw(map[string]any{"type": "client_message", "client_id": "background", "stream_id": "shared", "message": map[string]any{"id": "resume-" + thread, "method": "thread/resume", "params": map[string]any{"threadId": thread, "model": "fixture-model", "cwd": "/fixture", "excludeTurns": true}}})
	cloudGatewaySend(t, backend, request)
	if got := cloudGatewayRead(t, native); !bytes.Equal(got, request) {
		t.Fatal("unchanged backend RPC was re-encoded")
	}
	f.nextSequence++
	response := cloudHarnessRaw(map[string]any{"type": "server_message", "client_id": "background", "stream_id": "shared", "seq_id": f.nextSequence, "message": map[string]any{"id": "resume-" + thread, "result": map[string]any{"thread": map[string]any{"id": thread, "cwd": "/fixture"}, "model": "fixture-model"}}})
	cloudGatewaySend(t, native, response)
	if got := cloudGatewayRead(t, backend); !bytes.Equal(got, response) {
		t.Fatal("visible native RPC was modified")
	}
	f.harness.mu.Lock()
	defer f.harness.mu.Unlock()
	for source := range f.harness.threads[thread].subscriptions {
		if strings.HasPrefix(source, "gateway/") {
			return source
		}
	}
	t.Fatal("gateway subscription was not registered")
	return ""
}

func TestCloudHarnessGatewayPrivateScopedRequestAndReplay(t *testing.T) {
	f := newCloudGatewayFixture(t)
	native, backend := f.connect(t)
	source := f.subscribe(t, native, backend, "chat")
	if f.subscribe(t, native, backend, "sibling") != source {
		t.Fatal("one native relay stream acquired different source identities")
	}
	params := f.gateway.ResumeParams(source, "chat")
	params["model"] = cloudHarnessRaw("mutated-copy")
	if cloudHarnessString(f.gateway.ResumeParams(source, "chat"), "model") != "fixture-model" {
		t.Fatal("resume parameters were not retained independently")
	}
	replies := make(chan cloudMessage, 1)
	f.harness.mu.Lock()
	f.harness.calls[`"cq-harness/fixture/1"`] = replies
	f.harness.mu.Unlock()
	request := []byte(`{"id":"cq-harness/fixture/1","method":"thread/resume","params":{"threadId":"chat","config":{"mcp_servers.fixture.enabled":false}}}`)
	if err := f.gateway.Write(source, request); err != nil {
		t.Fatal(err)
	}
	injected, ok := decodeCloudGatewayEnvelope(cloudGatewayRead(t, native))
	if !ok || injected.ThreadID != "chat" || injected.Sequence != 0 || string(cloudHarnessObject(injected.Message["params"])["excludeTurns"]) != "true" || string(cloudHarnessObject(injected.Message["params"])["config"]) != `{"mcp_servers.fixture.enabled":false}` {
		t.Fatal("private request lost compiled config, bounded history or stream scope")
	}
	response := []byte(`{"type":"server_message","client_id":"background","stream_id":"shared","seq_id":3,"message":{"id":"cq-harness/fixture/1","result":{"status":"unsubscribed"}}}`)
	cloudGatewaySend(t, native, response)
	first := cloudGatewayRead(t, backend)
	var pong cloudMessage
	_ = json.Unmarshal(first, &pong)
	if cloudHarnessString(pong, "type") != "pong" || string(pong["seq_id"]) != "3" || bytes.Contains(first, []byte("cq-harness")) {
		t.Fatal("private reply escaped or its sequence was lost")
	}
	select {
	case <-replies:
	case <-time.After(time.Second):
		t.Fatal("private RPC waiter did not receive native response")
	}
	cloudGatewaySend(t, native, response)
	if replay := cloudGatewayRead(t, backend); !bytes.Equal(replay, first) {
		t.Fatal("private replay was not replaced deterministically")
	}
}

func TestCloudHarnessGatewayReconnectRetainedSubscriptions(t *testing.T) {
	f := newCloudGatewayFixture(t)
	native, backend := f.connect(t)
	source := f.subscribe(t, native, backend, "chat")
	_ = native.Close()
	_ = backend.Close()
	native, backend = f.connect(t)
	ping := []byte(`{"type":"ping","client_id":"background","stream_id":"shared"}`)
	cloudGatewaySend(t, backend, ping)
	if !bytes.Equal(cloudGatewayRead(t, native), ping) {
		t.Fatal("reconnect ping changed")
	}
	if err := f.gateway.Write(source, []byte(`{"id":"cq-harness/fixture/2","method":"thread/unsubscribe","params":{"threadId":"chat"}}`)); err != nil {
		t.Fatal("reconnected source did not rebind its native writer")
	}
	_ = cloudGatewayRead(t, native)
	f.harness.mu.Lock()
	owned := f.harness.threads["chat"].subscriptions[source]
	f.harness.mu.Unlock()
	if !owned {
		t.Fatal("network disconnect detached a native subscription")
	}
	f.harness.mu.Lock()
	f.harness.threads["chat"].pendingTurns = 2
	f.harness.threads["chat"].pendingSettings = 2
	for _, entry := range []struct{ id, method, source string }{
		{"gateway-turn", "turn/start", source}, {"gateway-settings", "thread/settings/update", source},
		{"stdio-turn", "turn/start", "stdio"}, {"stdio-settings", "thread/settings/update", "stdio"},
	} {
		f.harness.requests[cloudHarnessRequestKey(entry.source, cloudHarnessRaw(entry.id))] = cloudHarnessRequest{method: entry.method, params: cloudMessage{"threadId": cloudHarnessRaw("chat")}}
	}
	f.harness.mu.Unlock()
	closed := []byte(`{"type":"client_closed","client_id":"background","stream_id":"shared"}`)
	cloudGatewaySend(t, backend, closed)
	if !bytes.Equal(cloudGatewayRead(t, native), closed) {
		t.Fatal("client close event changed")
	}
	if err := f.gateway.Write(source, []byte(`{"id":"cq-harness/fixture/3","method":"thread/unsubscribe","params":{"threadId":"chat"}}`)); err == nil {
		t.Fatal("closed native client retained a writable source")
	}
	f.harness.mu.Lock()
	owned = f.harness.threads["chat"].subscriptions[source]
	turns, settings := f.harness.threads["chat"].pendingTurns, f.harness.threads["chat"].pendingSettings
	f.harness.mu.Unlock()
	if owned || f.gateway.ResumeParams(source, "chat") != nil {
		t.Fatal("client_closed retained its subscription or resume parameters")
	}
	if turns != 1 || settings != 1 {
		t.Fatal("client_closed retained stale request counters or removed another source's pending requests")
	}
}

func TestCloudHarnessGatewayUnknownFramesRemainOpaque(t *testing.T) {
	f := newCloudGatewayFixture(t)
	native, backend := f.connect(t)
	for _, raw := range [][]byte{
		[]byte(` { "type": "future_event", "sensitive": "fixture" } `),
		[]byte(`{"type":"client_message_chunk","client_id":"background","stream_id":"chunked","seq_id":1,"segment_id":0,"segment_count":1,"message_size_bytes":2,"message_chunk_base64":"e30="}`),
	} {
		cloudGatewaySend(t, backend, raw)
		if !bytes.Equal(cloudGatewayRead(t, native), raw) {
			t.Fatal("opaque backend frame changed")
		}
		cloudGatewaySend(t, native, raw)
		if !bytes.Equal(cloudGatewayRead(t, backend), raw) {
			t.Fatal("opaque native frame changed")
		}
	}
	f.harness.mu.Lock()
	defer f.harness.mu.Unlock()
	if len(f.harness.threads) != 0 || len(f.harness.requests) != 0 {
		t.Fatal("unsupported frames established harness ownership")
	}
}

func TestCloudHarnessGatewayInvalidControlCannotDetach(t *testing.T) {
	f := newCloudGatewayFixture(t)
	native, backend := f.connect(t)
	source := f.subscribe(t, native, backend, "chat")
	invalid := []byte(`{"type":"client_closed","client_id":"background","stream_id":"shared","seq_id":"private-secret"}`)
	cloudGatewaySend(t, backend, invalid)
	if !bytes.Equal(cloudGatewayRead(t, native), invalid) {
		t.Fatal("invalid control frame changed")
	}
	if err := f.gateway.Write(source, []byte(`{"id":"cq-harness/fixture/2","method":"thread/unsubscribe","params":{"threadId":"chat"}}`)); err != nil {
		t.Fatal("invalid control frame detached native subscription")
	}
	_ = cloudGatewayRead(t, native)
	for _, raw := range []string{
		`{"id":"cq-harness/other/1","method":"thread/resume","params":{"threadId":"chat"}}`,
		`{"id":"cq-harness/fixture/1","method":"thread/archive","params":{"threadId":"chat"}}`,
		`{"id":"cq-harness/fixture/1","method":"thread/resume","params":{"threadId":"private-secret\n"}}`,
	} {
		if err := f.gateway.Write(source, []byte(raw)); err == nil || strings.Contains(err.Error(), "private-secret") {
			t.Fatal("invalid private request was not rejected with a redacted error")
		}
	}
}

func TestCloudHarnessGatewayForwardedPingPongAndClose(t *testing.T) {
	f := newCloudGatewayFixture(t)
	native, backend := f.connect(t)
	ping, pong := "", ""
	native.SetPingHandler(func(data string) error {
		ping = data
		return native.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(time.Second))
	})
	backend.SetPongHandler(func(data string) error { pong = data; return nil })
	if err := backend.WriteControl(websocket.PingMessage, []byte("fixture-ping"), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	cloudGatewaySend(t, backend, []byte(`{"opaque":"native"}`))
	_ = cloudGatewayRead(t, native)
	cloudGatewaySend(t, native, []byte(`{"opaque":"backend"}`))
	_ = cloudGatewayRead(t, backend)
	if ping != "fixture-ping" || pong != "fixture-ping" {
		t.Fatal("gateway consumed a peer Ping/Pong control instead of forwarding it")
	}
	if err := backend.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "fixture-close"), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	_ = native.SetReadDeadline(time.Now().Add(time.Second))
	_, _, err := native.ReadMessage()
	var closed *websocket.CloseError
	if !errors.As(err, &closed) || closed.Code != websocket.CloseNormalClosure || closed.Text != "fixture-close" {
		t.Fatal("gateway lost peer close code/reason")
	}
}

func TestCloudHarnessGatewayPreparedRPCPreservedEnvelope(t *testing.T) {
	f := newCloudGatewayFixture(t)
	f.harness.b.deps.readHarnessPolicy = func() (harness.Policy, error) {
		return harness.Policy{Version: 1, Enabled: true}, nil
	}
	f.harness.b.local = &cloudLocal{input: cloudTestWriter{write: func(raw []byte) (int, error) {
		var request cloudMessage
		_ = json.Unmarshal(raw, &request)
		f.harness.observeFrom("stdio", cloudHarnessRaw(map[string]any{"id": request["id"], "result": map[string]any{"config": map[string]any{"model": "glm-5.3", "features": map[string]bool{"plugins": true}, "mcp_servers": map[string]any{"fixture": map[string]any{"enabled": true}}}, "layers": []any{}}}))
		return len(raw), nil
	}}}
	native, backend := f.connect(t)
	raw := []byte(`{"type":"client_message","client_id":"background","stream_id":"shared","seq_id":51,"cursor":"fixture-cursor","future":{"keep":true},"message":{"id":"start","method":"thread/start","params":{"model":"glm-5.3","cwd":"/fixture"}}}`)
	cloudGatewaySend(t, backend, raw)
	got, ok := decodeCloudGatewayEnvelope(cloudGatewayRead(t, native))
	if !ok || got.Sequence != 51 || string(got.Fields["cursor"]) != `"fixture-cursor"` || string(got.Fields["future"]) != `{"keep":true}` || string(got.ID) != `"start"` {
		t.Fatal("prepared RPC lost backend envelope metadata")
	}
	config := cloudHarnessObject(cloudHarnessObject(got.Message["params"])["config"])
	if string(config["mcp_servers.fixture.enabled"]) != "false" {
		t.Fatal("backend-originated first start bypassed harness preparation")
	}
}

func TestCloudHarnessGatewayPreparationFailureRetainedSubscription(t *testing.T) {
	f := newCloudGatewayFixture(t)
	native, backend := f.connect(t)
	source := f.subscribe(t, native, backend, "chat")
	f.harness.b.deps.readHarnessPolicy = func() (harness.Policy, error) {
		return harness.Policy{}, errors.New("private-secret")
	}
	cloudGatewaySend(t, backend, []byte(`{"type":"client_message","client_id":"background","stream_id":"shared","message":{"id":"start","method":"thread/start","params":{"model":"glm-5.3","cwd":"/fixture"}}}`))
	_ = backend.SetReadDeadline(time.Now().Add(time.Second))
	_, _, err := backend.ReadMessage()
	var closed *websocket.CloseError
	if !errors.As(err, &closed) || closed.Code != websocket.ClosePolicyViolation || strings.Contains(closed.Text, "private-secret") {
		t.Fatal("preparation failure did not close the relay with a bounded error")
	}
	_ = native.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := native.ReadMessage(); err == nil {
		t.Fatal("unprepared RPC reached native app-server")
	}
	f.harness.mu.Lock()
	defer f.harness.mu.Unlock()
	if !f.harness.threads["chat"].subscriptions[source] {
		t.Fatal("preparation failure detached native client subscriptions")
	}
}

func TestCloudHarnessGatewayRepeatedInitializeResetOnlyExistingSource(t *testing.T) {
	f := newCloudGatewayFixture(t)
	native, backend := f.connect(t)
	source := f.subscribe(t, native, backend, "chat")
	other := []byte(`{"type":"client_message","client_id":"other-background","stream_id":"other","message":{"id":"other-resume","method":"thread/resume","params":{"threadId":"sibling","excludeTurns":true}}}`)
	cloudGatewaySend(t, backend, other)
	_ = cloudGatewayRead(t, native)
	cloudGatewaySend(t, native, []byte(`{"type":"server_message","client_id":"other-background","stream_id":"other","seq_id":1,"message":{"id":"other-resume","result":{"thread":{"id":"sibling"}}}}`))
	_ = cloudGatewayRead(t, backend)
	f.harness.mu.Lock()
	cloudHarnessSubscription(f.harness.threads["chat"], "stdio", true)
	f.harness.threads["chat"].pendingTurns = 2
	f.harness.threads["chat"].pendingSettings = 2
	for _, entry := range []struct{ id, method, source string }{
		{"gateway-turn", "turn/start", source}, {"gateway-settings", "thread/settings/update", source},
		{"stdio-turn", "turn/start", "stdio"}, {"stdio-settings", "thread/settings/update", "stdio"},
	} {
		f.harness.requests[cloudHarnessRequestKey(entry.source, cloudHarnessRaw(entry.id))] = cloudHarnessRequest{method: entry.method, params: cloudMessage{"threadId": cloudHarnessRaw("chat")}}
	}
	f.harness.mu.Unlock()
	initialize := []byte(`{"type":"client_message","client_id":"background","stream_id":"shared","message":{"id":"init-again","method":"initialize","params":{"clientInfo":{"name":"fixture","version":"1"}}}}`)
	cloudGatewaySend(t, backend, initialize)
	if !bytes.Equal(cloudGatewayRead(t, native), initialize) {
		t.Fatal("reinitialisation request changed")
	}
	f.harness.mu.Lock()
	chat, sibling := f.harness.threads["chat"], f.harness.threads["sibling"]
	reset, stdio, siblingOwned := !chat.subscriptions[source], chat.subscriptions["stdio"], sibling.owned
	turns, settings, pending := chat.pendingTurns, chat.pendingSettings, len(f.harness.requests)
	f.harness.mu.Unlock()
	if !reset || !stdio || !siblingOwned || turns != 1 || settings != 1 || pending != 2 {
		t.Fatal("reinitialisation retained replaced ownership or changed another source/pending counter")
	}
	if f.gateway.ResumeParams(source, "chat") != nil {
		t.Fatal("reinitialisation retained replaced client's resume parameters")
	}
	if err := f.gateway.Write(source, []byte(`{"id":"cq-harness/fixture/2","method":"thread/unsubscribe","params":{"threadId":"chat"}}`)); err != nil {
		t.Fatal("replacement logical client did not rebind its native writer")
	}
	_ = cloudGatewayRead(t, native)
}

func TestCloudHarnessGatewayOutputReplayCannotClearCurrentActivity(t *testing.T) {
	f := newCloudGatewayFixture(t)
	f.harness.threads["chat"] = &cloudHarnessThread{id: "chat", owned: true, baseline: cloudMessage{}}
	session := &cloudHarnessGatewaySession{}
	completed := []byte(`{"type":"server_message","client_id":"background","stream_id":"stream","seq_id":1,"message":{"method":"turn/completed","params":{"threadId":"chat","turn":{"id":"old-turn"}}}}`)
	if _, err := f.gateway.message(session, false, completed); err != nil {
		t.Fatal(err)
	}
	f.harness.observe([]byte(`{"method":"turn/started","params":{"threadId":"chat","turn":{"id":"current-turn"}}}`))
	if _, err := f.gateway.message(session, false, completed); err != nil {
		t.Fatal(err)
	}
	if !f.harness.threads["chat"].active {
		t.Fatal("replayed native completion cleared the current turn's active guard")
	}
}

func TestCloudHarnessGatewayInputReplayDidNotRepeatPreparation(t *testing.T) {
	f := newCloudGatewayFixture(t)
	native, backend := f.connect(t)
	source := f.subscribe(t, native, backend, "chat")
	raw := []byte(`{"type":"client_message","client_id":"background","stream_id":"shared","seq_id":7,"message":{"id":"settings","method":"thread/settings/update","params":{"threadId":"chat"}}}`)
	for range 2 {
		cloudGatewaySend(t, backend, raw)
		if !bytes.Equal(cloudGatewayRead(t, native), raw) {
			t.Fatal("unchanged replay changed native wire payload")
		}
	}
	f.harness.mu.Lock()
	pending := f.harness.threads["chat"].pendingSettings
	f.harness.mu.Unlock()
	if pending != 1 {
		t.Fatal("replayed input duplicated pending settings counters")
	}
	cloudGatewaySend(t, native, []byte(`{"type":"server_message","client_id":"background","stream_id":"shared","seq_id":2,"message":{"id":"settings","result":{}}}`))
	_ = cloudGatewayRead(t, backend)
	cloudGatewaySend(t, backend, raw)
	_ = cloudGatewayRead(t, native)
	f.harness.mu.Lock()
	defer f.harness.mu.Unlock()
	if f.harness.threads["chat"].pendingSettings != 0 || len(f.harness.requests) != 0 || !f.harness.threads["chat"].subscriptions[source] {
		t.Fatal("completed input replay recreated pending work or lost source ownership")
	}
}

func TestCloudHarnessGatewayOldSessionCannotReplaceRecoveredWriter(t *testing.T) {
	f := newCloudGatewayFixture(t)
	oldNative, oldBackend := f.connect(t)
	source := f.subscribe(t, oldNative, oldBackend, "chat")
	f.harness.mu.Lock()
	epoch := f.harness.threads["chat"].sourceEpoch
	f.harness.mu.Unlock()
	newNative, newBackend := f.connect(t)
	ping := []byte(`{"type":"ping","client_id":"background","stream_id":"shared"}`)
	cloudGatewaySend(t, newBackend, ping)
	_ = cloudGatewayRead(t, newNative)
	cloudGatewaySend(t, newBackend, ping)
	_ = cloudGatewayRead(t, newNative)
	cloudGatewaySend(t, oldBackend, ping)
	_ = cloudGatewayRead(t, oldNative)
	_ = oldNative.Close()
	_ = oldBackend.Close()
	if err := f.gateway.Write(source, []byte(`{"id":"cq-harness/fixture/2","method":"thread/unsubscribe","params":{"threadId":"chat"}}`)); err != nil {
		t.Fatal("old physical session displaced recovered writer")
	}
	_ = cloudGatewayRead(t, newNative)
	f.harness.mu.Lock()
	defer f.harness.mu.Unlock()
	if f.harness.threads["chat"].sourceEpoch != epoch+1 {
		t.Fatal("recovery epoch advanced for repeated traffic or an older session")
	}
}

func TestCloudHarnessGatewayRetriedPreparedRPCAfterReconnect(t *testing.T) {
	f := newCloudGatewayFixture(t)
	var enabled atomic.Bool
	var reads atomic.Int32
	enabled.Store(true)
	f.harness.b.deps.readHarnessPolicy = func() (harness.Policy, error) {
		return harness.Policy{Version: 1, Enabled: enabled.Load()}, nil
	}
	f.harness.b.local = &cloudLocal{input: cloudTestWriter{write: func(raw []byte) (int, error) {
		reads.Add(1)
		var request cloudMessage
		_ = json.Unmarshal(raw, &request)
		f.harness.observeFrom("stdio", cloudHarnessRaw(map[string]any{"id": request["id"], "result": map[string]any{"config": map[string]any{"model": "glm-5.3", "features": map[string]bool{"plugins": true}, "mcp_servers": map[string]any{"fixture": map[string]any{"enabled": true}}}, "layers": []any{}}}))
		return len(raw), nil
	}}}
	native, backend := f.connect(t)
	raw := []byte(`{"type":"client_message","client_id":"background","stream_id":"shared","seq_id":11,"cursor":"original","message":{"id":"start","method":"thread/start","params":{"model":"glm-5.3","cwd":"/fixture"}}}`)
	cloudGatewaySend(t, backend, raw)
	first, ok := decodeCloudGatewayEnvelope(cloudGatewayRead(t, native))
	if !ok || bytes.Equal(first.Fields["message"], cloudHarnessObject(raw)["message"]) {
		t.Fatal("fixture did not prepare the managed request")
	}
	_ = native.Close()
	_ = backend.Close()
	enabled.Store(false)
	native, backend = f.connect(t)
	replayed := bytes.Replace(raw, []byte(`"cursor":"original"`), []byte(`"cursor":"replayed"`), 1)
	cloudGatewaySend(t, backend, replayed)
	second, ok := decodeCloudGatewayEnvelope(cloudGatewayRead(t, native))
	if !ok || !bytes.Equal(first.Fields["message"], second.Fields["message"]) || string(second.Fields["cursor"]) != `"replayed"` || reads.Load() != 1 {
		t.Fatal("reconnect prepared the same input twice or lost transformed RPC/cursor")
	}
	cloudGatewaySend(t, native, []byte(`{"type":"server_message","client_id":"background","stream_id":"shared","seq_id":1,"message":{"id":"start","result":{"thread":{"id":"chat"},"model":"glm-5.3"}}}`))
	_ = cloudGatewayRead(t, backend)
	f.gateway.mu.Lock()
	remaining := f.gateway.preparedCount
	f.gateway.mu.Unlock()
	if remaining != 0 {
		t.Fatal("correlated native reply retained prepared request bytes")
	}
	cloudGatewaySend(t, backend, raw)
	_ = cloudGatewayRead(t, native)
	f.harness.mu.Lock()
	pending := len(f.harness.requests)
	f.harness.mu.Unlock()
	if pending != 0 || reads.Load() != 1 {
		t.Fatal("completed request replay recreated harness pending work")
	}
}

func TestCloudHarnessGatewayPreparedCapacityReservedBeforePreparation(t *testing.T) {
	f := newCloudGatewayFixture(t)
	f.gateway.preparedCount = cloudHarnessGatewayPreparedLimit - 1
	f.harness.b.deps.readHarnessPolicy = func() (harness.Policy, error) { return harness.Policy{Version: 1, Enabled: true}, nil }
	started, release := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	t.Cleanup(finish)
	f.harness.b.local = &cloudLocal{input: cloudTestWriter{write: func(raw []byte) (int, error) {
		started <- struct{}{}
		<-release
		var request cloudMessage
		_ = json.Unmarshal(raw, &request)
		f.harness.observeFrom("stdio", cloudHarnessRaw(map[string]any{"id": request["id"], "result": map[string]any{"config": map[string]any{"features": map[string]bool{"plugins": true}}, "layers": []any{}}}))
		return len(raw), nil
	}}}
	request := []byte(`{"type":"client_message","client_id":"background","stream_id":"first","seq_id":1,"message":{"id":"first","method":"thread/start","params":{"model":"glm-5.3","cwd":"/fixture"}}}`)
	firstDone, secondDone := make(chan error, 1), make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				firstDone <- errors.New("fixture preparation panicked")
			}
		}()
		_, err := f.gateway.message(&cloudHarnessGatewaySession{}, true, request)
		firstDone <- err
	}()
	<-started
	go func() {
		defer func() {
			if recover() != nil {
				secondDone <- errors.New("fixture preparation panicked")
			}
		}()
		_, err := f.gateway.message(&cloudHarnessGatewaySession{}, true, bytes.ReplaceAll(request, []byte("first"), []byte("second")))
		secondDone <- err
	}()
	select {
	case err := <-secondDone:
		if err == nil {
			t.Error("prepared request capacity was exceeded")
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("preparation did not reserve its bounded cache capacity before blocking")
	}
	finish()
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestCloudHarnessGatewayOversizedPreparedRPCWasNotRetained(t *testing.T) {
	f := newCloudGatewayFixture(t)
	f.harness.b.deps.readHarnessPolicy = func() (harness.Policy, error) { return harness.Policy{Version: 1, Enabled: true}, nil }
	f.harness.b.local = &cloudLocal{input: cloudTestWriter{write: func(raw []byte) (int, error) {
		var request cloudMessage
		_ = json.Unmarshal(raw, &request)
		f.harness.observeFrom("stdio", cloudHarnessRaw(map[string]any{"id": request["id"], "result": map[string]any{"config": map[string]any{"features": map[string]bool{"plugins": true}}, "layers": []any{}}}))
		return len(raw), nil
	}}}
	base := `{"type":"client_message","client_id":"background","stream_id":"stream","seq_id":1,"message":{"id":"start","method":"thread/start","params":{"model":"glm-5.3","cwd":"/fixture","padding":""}}}`
	raw := []byte(strings.Replace(base, `"padding":""`, `"padding":"`+strings.Repeat("x", cloudGatewayMaxEnvelopeBytes-len(base))+`"`, 1))
	if _, err := f.gateway.message(&cloudHarnessGatewaySession{}, true, raw); err == nil {
		t.Fatal("oversized prepared direct envelope was accepted")
	}
	f.gateway.mu.Lock()
	retained := f.gateway.preparedCount
	f.gateway.mu.Unlock()
	f.harness.mu.Lock()
	pending := len(f.harness.requests)
	f.harness.mu.Unlock()
	if retained != 0 || pending != 0 {
		t.Fatal("oversized prepared RPC retained bytes or stranded pending work")
	}
}

func TestCloudHarnessGatewayConcurrentReplayWaitedForPreparedRPC(t *testing.T) {
	f := newCloudGatewayFixture(t)
	var enabled atomic.Bool
	enabled.Store(true)
	f.harness.b.deps.readHarnessPolicy = func() (harness.Policy, error) { return harness.Policy{Version: 1, Enabled: enabled.Load()}, nil }
	started, release := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	t.Cleanup(finish)
	f.harness.b.local = &cloudLocal{input: cloudTestWriter{write: func(raw []byte) (int, error) {
		started <- struct{}{}
		<-release
		var request cloudMessage
		_ = json.Unmarshal(raw, &request)
		f.harness.observeFrom("stdio", cloudHarnessRaw(map[string]any{"id": request["id"], "result": map[string]any{"config": map[string]any{"features": map[string]bool{"plugins": true}}, "layers": []any{}}}))
		return len(raw), nil
	}}}
	request := []byte(`{"type":"client_message","client_id":"background","stream_id":"stream","seq_id":1,"message":{"id":"start","method":"thread/start","params":{"model":"glm-5.3","cwd":"/fixture"}}}`)
	firstDone, replayDone := make(chan []byte, 1), make(chan []byte, 1)
	go func() {
		defer func() {
			if recover() != nil {
				firstDone <- nil
			}
		}()
		raw, _ := f.gateway.message(&cloudHarnessGatewaySession{}, true, request)
		firstDone <- raw
	}()
	<-started
	enabled.Store(false)
	recovered := &cloudHarnessGatewaySession{}
	go func() {
		defer func() {
			if recover() != nil {
				replayDone <- nil
			}
		}()
		raw, _ := f.gateway.message(recovered, true, request)
		replayDone <- raw
	}()
	deadline := time.Now().Add(time.Second)
	for {
		f.gateway.mu.Lock()
		bound := f.gateway.streams[cloudHarnessGatewaySource("background", "stream")].writer == recovered
		f.gateway.mu.Unlock()
		if bound {
			break
		}
		if time.Now().After(deadline) {
			finish()
			t.Fatal("reconnect did not bind while preparation waited")
		}
		time.Sleep(time.Millisecond)
	}
	finish()
	first, replay := <-firstDone, <-replayDone
	if first == nil || !bytes.Equal(first, replay) || bytes.Equal(first, request) {
		t.Fatal("in-flight replay repeated preparation or lost the original compiled RPC")
	}
	f.harness.mu.Lock()
	defer f.harness.mu.Unlock()
	if len(f.harness.requests) != 1 {
		t.Fatal("in-flight replay duplicated pending requests")
	}
}

func TestCloudHarnessGatewayResumeCandidatesStayedScopedWithoutResult(t *testing.T) {
	f := newCloudGatewayFixture(t)
	native, backend := f.connect(t)
	for _, entry := range []struct{ client, stream, thread string }{
		{"first", "first", "chat"}, {"second", "second", "chat"}, {"sibling", "sibling", "sibling"},
	} {
		raw := cloudHarnessRaw(map[string]any{"type": "client_message", "client_id": entry.client, "stream_id": entry.stream, "message": map[string]any{"id": "resume-" + entry.client, "method": "thread/resume", "params": map[string]any{"threadId": entry.thread}}})
		cloudGatewaySend(t, backend, raw)
		_ = cloudGatewayRead(t, native)
	}
	// A long-history response stays opaque. Merely seeing the resume request
	// makes a source a candidate; it does not establish a successful owner.
	opaque := []byte(`{"type":"server_message_chunk","client_id":"first","stream_id":"first","seq_id":1,"segment_id":0,"segment_count":2,"message_size_bytes":100,"message_chunk_base64":"e30="}`)
	cloudGatewaySend(t, native, opaque)
	if !bytes.Equal(cloudGatewayRead(t, backend), opaque) {
		t.Fatal("candidate discovery changed an unsupported response chunk")
	}
	chat, sibling := f.gateway.ResumeSources("chat"), f.gateway.ResumeSources("sibling")
	if len(chat) != 2 || !slices.IsSorted(chat) || len(sibling) != 1 || slices.Contains(chat, sibling[0]) || len(f.gateway.ResumeSources("unknown")) != 0 {
		t.Fatal("resume candidates were not deterministic and scoped to their requested chat")
	}
	first := slices.Clone(chat)
	chat[0] = "mutated-copy"
	if !slices.Equal(f.gateway.ResumeSources("chat"), first) {
		t.Fatal("caller mutated retained source candidates")
	}
	f.harness.mu.Lock()
	owners := len(f.harness.threads)
	f.harness.mu.Unlock()
	if owners != 0 {
		t.Fatal("request-only candidate was inferred to be a successful subscription")
	}
	_ = native.Close()
	_ = backend.Close()
	if !slices.Equal(f.gateway.ResumeSources("chat"), first) {
		t.Fatal("network disconnect discarded logical resume candidates")
	}
	native, backend = f.connect(t)
	cloudGatewaySend(t, backend, []byte(`{"type":"client_message","client_id":"first","stream_id":"first","message":{"id":"initialize","method":"initialize","params":{"clientInfo":{"name":"fixture","version":"1"}}}}`))
	_ = cloudGatewayRead(t, native)
	if len(f.gateway.ResumeSources("chat")) != 1 || !slices.Equal(f.gateway.ResumeSources("sibling"), sibling) {
		t.Fatal("initialise reset changed another source's resume candidates")
	}
	cloudGatewaySend(t, backend, []byte(`{"type":"client_closed","client_id":"second","stream_id":"second"}`))
	_ = cloudGatewayRead(t, native)
	if len(f.gateway.ResumeSources("chat")) != 0 || !slices.Equal(f.gateway.ResumeSources("sibling"), sibling) {
		t.Fatal("client close retained its candidate or cleared a sibling source")
	}
}

func TestCloudHarnessGatewayNewCandidateWokeOwnedTargetOnce(t *testing.T) {
	f := newCloudGatewayFixture(t)
	f.harness.threads["chat"] = &cloudHarnessThread{id: "chat", owned: true, subscriptions: map[string]bool{"stdio": true}, baseline: cloudMessage{}, sourceEpoch: 7}
	native, backend := f.connect(t)
	for _, entry := range []struct{ client, stream, thread string }{
		{"first", "first", "chat"}, {"first", "first", "chat"}, {"first", "first", "unknown"}, {"second", "second", "chat"},
	} {
		raw := cloudHarnessRaw(map[string]any{"type": "client_message", "client_id": entry.client, "stream_id": entry.stream, "message": map[string]any{"id": "resume-" + entry.client + entry.thread, "method": "thread/resume", "params": map[string]any{"threadId": entry.thread}}})
		cloudGatewaySend(t, backend, raw)
		_ = cloudGatewayRead(t, native)
	}
	f.harness.mu.Lock()
	defer f.harness.mu.Unlock()
	chat := f.harness.threads["chat"]
	if chat.sourceEpoch != 9 || len(chat.subscriptions) != 1 || !chat.subscriptions["stdio"] || f.harness.threads["unknown"] != nil {
		t.Fatal("new candidates did not wake their owned target exactly once or invented a subscription")
	}
}
