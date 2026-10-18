package dots

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func celsiusTestOwners() map[string]Account {
	return map[string]Account{
		"main-room":   {Alias: "primary", ViewerIDs: map[string]bool{"main-user": true}},
		"worker-room": {Alias: "worker", ViewerIDs: map[string]bool{"worker-user": true}},
		"main-root":   {Alias: "primary"},
	}
}
func celsiusTestBroker() *CelsiusBroker {
	return NewCelsiusBroker(&Settings{SystemAlias: "primary", IntegrationMode: "native", Dots: map[string]Dot{"main": {OwnerAlias: "primary"}, "worker": {OwnerAlias: "worker"}}}, nil)
}

func TestCelsiusTargetsAreOwnerPinned(t *testing.T) {
	owners := celsiusTestOwners()
	for _, test := range []struct {
		command map[string]any
		want    []string
		failure bool
	}{
		{map[string]any{"type": "connect"}, []string{"primary", "worker"}, false},
		{map[string]any{"type": "presence"}, []string{"primary", "worker"}, false},
		{map[string]any{"type": "subscribe", "topic_id": "calpico-chatgpt-messaging"}, []string{"primary", "worker"}, false},
		{map[string]any{"type": "subscribe", "topic_id": "calpico-room-live:worker-room"}, []string{"worker"}, false},
		{map[string]any{"type": "unsubscribe", "topic_id": "calpico-room-live:main-room"}, []string{"primary"}, false},
		{map[string]any{"type": "subscribe", "topic_id": "calpico-room-live:unknown"}, nil, true},
		{map[string]any{"type": "future/mutation", "topic_id": "worker-room"}, []string{"primary"}, false},
	} {
		got, err := celsiusTargets(test.command, "primary", owners)
		if (err != nil) != test.failure {
			t.Fatalf("targets error = %v", err)
		}
		data, _ := json.Marshal(got)
		want, _ := json.Marshal(test.want)
		if string(data) != string(want) {
			t.Fatalf("targets = %s, want %s", data, want)
		}
	}
}

func TestCelsiusGrantRequiresEveryConfiguredOwnerAndValidOrigin(t *testing.T) {
	b := celsiusTestBroker()
	owners := celsiusTestOwners()
	resolver := func(context.Context, string) (string, error) { return "", errors.New("must remain deferred") }
	endpoint, err := b.Issue(context.Background(), map[string]string{"primary": "wss://ws.chatgpt.com/primary"}, owners, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(endpoint, "wss://ws.chatgpt.com/cq-owner-events/") || len(strings.TrimPrefix(endpoint, "wss://ws.chatgpt.com/cq-owner-events/")) != 43 {
		t.Fatalf("invalid local grant: %q", endpoint)
	}
	for _, urls := range []map[string]string{
		{"primary": "wss://ws.chatgpt.com/primary"},
		{"primary": "wss://ws.chatgpt.com/primary", "worker": "wss://other.invalid/worker"},
		{"primary": "wss://ws.chatgpt.com:444/primary", "worker": "wss://ws.chatgpt.com/worker"},
	} {
		if _, err := b.Issue(context.Background(), urls, owners, nil); err == nil {
			t.Fatal("accepted incomplete or untrusted owner URLs")
		}
	}
	for len(b.grants) < 16 {
		if _, err := b.Issue(context.Background(), map[string]string{"primary": "wss://ws.chatgpt.com/primary"}, owners, resolver); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.Issue(context.Background(), map[string]string{"primary": "wss://ws.chatgpt.com/primary"}, owners, resolver); err == nil {
		t.Fatal("unbounded grant cache")
	}
	b.now = func() time.Time { return time.Now().Add(1501 * time.Second) }
	if _, err := b.Issue(context.Background(), map[string]string{"primary": "wss://ws.chatgpt.com/primary"}, owners, resolver); err != nil {
		t.Fatal("expired grants were retained:", err)
	}
}

func TestCelsiusSecondaryEventsPreserveOwnerAndPrimaryCursor(t *testing.T) {
	original := map[string]any{"type": "message", "topic_id": "calpico-chatgpt-messaging", "offset": "secondary-cursor", "payload": map[string]any{"type": "calpico-message-add", "payload": map[string]any{"room_id": "worker-room", "message": map[string]any{"account_user_id": "worker-user", "content": map[string]any{"text": "synthetic"}}}}}
	owners := celsiusTestOwners()
	got := celsiusSecondaryFrame(original, owners, "worker")
	if got == nil {
		t.Fatal("owned event dropped")
	}
	if _, ok := got["offset"]; ok {
		t.Fatal("secondary cursor escaped")
	}
	message := got["payload"].(map[string]any)["payload"].(map[string]any)["message"].(map[string]any)
	if message["is_self"] != true {
		t.Fatal("viewer presentation missing")
	}
	precise := celsiusClone(map[string]any{"value": json.Number("9007199254740993")})
	if precise["value"] != json.Number("9007199254740993") {
		t.Fatal("cloning changed an exact JSON integer")
	}
	before := original["payload"].(map[string]any)["payload"].(map[string]any)["message"].(map[string]any)
	if _, exists := before["is_self"]; exists {
		t.Fatal("source event mutated")
	}
	if celsiusSecondaryFrame(original, owners, "other") != nil {
		t.Fatal("cross-owner event accepted")
	}
	conflict := celsiusClone(original)
	conflict["payload"].(map[string]any)["payload"].(map[string]any)["thread_id"] = "main-root"
	if celsiusSecondaryFrame(conflict, owners, "worker") != nil {
		t.Fatal("conflicting nested resource accepted")
	}
}

type celsiusFakeSocket struct {
	input   chan []byte
	writes  chan []byte
	closed  chan struct{}
	once    sync.Once
	onWrite func([]byte) error
}

func newCelsiusFakeSocket() *celsiusFakeSocket {
	return &celsiusFakeSocket{input: make(chan []byte, 256), writes: make(chan []byte, 256), closed: make(chan struct{})}
}
func (s *celsiusFakeSocket) ReadMessage() (int, []byte, error) {
	select {
	case data := <-s.input:
		return websocket.TextMessage, data, nil
	case <-s.closed:
		return 0, nil, io.EOF
	}
}
func (s *celsiusFakeSocket) WriteMessage(_ int, data []byte) error {
	select {
	case <-s.closed:
		return io.EOF
	default:
	}
	copy := append([]byte{}, data...)
	s.writes <- copy
	if s.onWrite != nil {
		return s.onWrite(copy)
	}
	return nil
}
func (s *celsiusFakeSocket) Close() error   { s.once.Do(func() { close(s.closed) }); return nil }
func (s *celsiusFakeSocket) push(value any) { data, _ := json.Marshal(value); s.input <- data }
func celsiusAutoAck(socket *celsiusFakeSocket, skipConnect bool) func([]byte) error {
	return func(data []byte) error {
		var batch []map[string]any
		if err := json.Unmarshal(data, &batch); err != nil {
			return err
		}
		for _, item := range batch {
			command := item["command"].(map[string]any)
			if skipConnect && command["type"] == "connect" {
				continue
			}
			socket.push([]any{map[string]any{"id": item["id"], "reply": map[string]any{"type": command["type"], "recovered": true, "catchups": []any{"owner-catchup"}}}})
		}
		return nil
	}
}
func celsiusCommand(id int, kind, topic string) map[string]any {
	command := map[string]any{"type": kind}
	if topic != "" {
		command["topic_id"] = topic
	}
	return map[string]any{"id": id, "command": command}
}
func celsiusReadBatch(t *testing.T, socket *celsiusFakeSocket) []map[string]any {
	t.Helper()
	select {
	case data := <-socket.writes:
		var value []map[string]any
		if err := json.Unmarshal(data, &value); err != nil {
			t.Fatal(err)
		}
		return value
	case <-time.After(2 * time.Second):
		t.Fatal("Celsius response timed out")
		return nil
	}
}
func celsiusAwaitReply(t *testing.T, client *celsiusFakeSocket, id int) map[string]any {
	t.Helper()
	for {
		for _, frame := range celsiusReadBatch(t, client) {
			if got, ok := celsiusID(frame["id"]); ok && got == int64(id) {
				return frame
			}
		}
	}
}
func celsiusRunTest(t *testing.T, b *CelsiusBroker, client *celsiusFakeSocket) {
	celsiusRunTestGrant(t, b, client, celsiusGrant{urls: map[string]string{"primary": "wss://ws.chatgpt.com/primary", "worker": "wss://ws.chatgpt.com/worker"}, owners: celsiusTestOwners()})
}
func celsiusRunTestGrant(t *testing.T, b *CelsiusBroker, client *celsiusFakeSocket, grant celsiusGrant) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.handle(ctx, client, grant)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("Celsius session did not stop")
		}
	})
}

func celsiusAwaitEvent(t *testing.T, events <-chan map[string]any, key string, want any) {
	t.Helper()
	for {
		select {
		case event := <-events:
			if event[key] == want {
				return
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("missing Celsius event %s=%v", key, want)
		}
	}
}

func TestCelsiusRetriesResolveFreshOwnerURLWithoutFailingPrimary(t *testing.T) {
	b := celsiusTestBroker()
	b.retryInitial, b.tickInterval = time.Millisecond, time.Millisecond
	events := make(chan map[string]any, 64)
	b.Event = func(event map[string]any) { events <- event }
	primary, worker, client := newCelsiusFakeSocket(), newCelsiusFakeSocket(), newCelsiusFakeSocket()
	primary.onWrite = celsiusAutoAck(primary, false)
	worker.onWrite = celsiusAutoAck(worker, false)
	var resolutions atomic.Int64
	resolver := func(context.Context, string) (string, error) {
		if resolutions.Add(1) == 1 {
			return "wss://ws.chatgpt.com/expired", nil
		}
		return "wss://ws.chatgpt.com/refreshed", nil
	}
	b.Dial = func(_ context.Context, endpoint string) (CelsiusSocket, error) {
		switch {
		case strings.HasSuffix(endpoint, "primary"):
			return primary, nil
		case strings.HasSuffix(endpoint, "expired"):
			return nil, errors.New("synthetic expired URL")
		case strings.HasSuffix(endpoint, "refreshed"):
			return worker, nil
		default:
			t.Errorf("unexpected endpoint")
			return nil, errors.New("unexpected endpoint")
		}
	}
	celsiusRunTestGrant(t, b, client, celsiusGrant{urls: map[string]string{"primary": "wss://ws.chatgpt.com/primary"}, owners: celsiusTestOwners(), resolve: resolver})
	celsiusAwaitEvent(t, events, "celsius_owner_unavailable", "worker")
	client.push([]any{celsiusCommand(99, "subscribe", "conversations")})
	celsiusAwaitReply(t, client, 99)
	celsiusAwaitEvent(t, events, "celsius_owner_connected", "worker")
	client.push([]any{celsiusCommand(100, "subscribe", "calpico-room-live:worker-room")})
	if reply := celsiusAwaitReply(t, client, 100)["reply"].(map[string]any); reply["type"] != "subscribe" {
		t.Fatalf("refreshed owner unavailable: %v", reply)
	}
	if resolutions.Load() != 2 {
		t.Fatalf("resolver calls = %d, want 2", resolutions.Load())
	}
	select {
	case <-primary.closed:
		t.Fatal("secondary connection failure closed primary")
	default:
	}
}

func TestCelsiusDeferredOwnerURLCannotChangeOrigin(t *testing.T) {
	b := celsiusTestBroker()
	b.retryInitial = time.Hour
	events := make(chan map[string]any, 64)
	b.Event = func(event map[string]any) { events <- event }
	primary, client := newCelsiusFakeSocket(), newCelsiusFakeSocket()
	primary.onWrite = celsiusAutoAck(primary, false)
	b.Dial = func(_ context.Context, endpoint string) (CelsiusSocket, error) {
		if endpoint != "wss://ws.chatgpt.com/primary" {
			t.Errorf("dialled untrusted owner origin")
		}
		return primary, nil
	}
	celsiusRunTestGrant(t, b, client, celsiusGrant{urls: map[string]string{"primary": "wss://ws.chatgpt.com/primary"}, owners: celsiusTestOwners(), resolve: func(context.Context, string) (string, error) {
		return "wss://other.invalid/owner", nil
	}})
	celsiusAwaitEvent(t, events, "celsius_owner_unavailable", "worker")
	client.push([]any{celsiusCommand(99, "subscribe", "conversations")})
	celsiusAwaitReply(t, client, 99)
}

func TestCelsiusDiagnosticPanicCannotBreakTransport(t *testing.T) {
	b := celsiusTestBroker()
	b.Event = func(map[string]any) { panic("synthetic event sink failure") }
	primary, worker, client := newCelsiusFakeSocket(), newCelsiusFakeSocket(), newCelsiusFakeSocket()
	primary.onWrite, worker.onWrite = celsiusAutoAck(primary, false), celsiusAutoAck(worker, false)
	b.Dial = func(_ context.Context, endpoint string) (CelsiusSocket, error) {
		if strings.HasSuffix(endpoint, "primary") {
			return primary, nil
		}
		return worker, nil
	}
	celsiusRunTest(t, b, client)
	client.push([]any{celsiusCommand(99, "subscribe", "conversations")})
	celsiusAwaitReply(t, client, 99)
}

func TestCelsiusShutdownClosesUnconsumedSecondaryDialResult(t *testing.T) {
	b := celsiusTestBroker()
	primary, worker, client := newCelsiusFakeSocket(), newCelsiusFakeSocket(), newCelsiusFakeSocket()
	primary.onWrite = celsiusAutoAck(primary, false)
	blocked, releaseMain, releaseDial, dialReturned := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	b.Event = func(event map[string]any) {
		if event["celsius_command_ack"] == "subscribe" {
			close(blocked)
			<-releaseMain
		}
	}
	b.Dial = func(_ context.Context, endpoint string) (CelsiusSocket, error) {
		if strings.HasSuffix(endpoint, "primary") {
			return primary, nil
		}
		<-releaseDial
		defer close(dialReturned)
		return worker, nil
	}
	celsiusRunTest(t, b, client)
	client.push([]any{celsiusCommand(99, "subscribe", "conversations")})
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("session did not reach ACK callback")
	}
	client.Close()
	close(releaseDial)
	<-dialReturned
	// Keep the consumer blocked while the successful dial result is enqueued.
	time.Sleep(10 * time.Millisecond)
	close(releaseMain)
	select {
	case <-worker.closed:
	case <-time.After(time.Second):
		t.Fatal("successful queued secondary socket leaked on session shutdown")
	}
}

func TestCelsiusQueuedRepeatedSubscriptionsAndUnsubscribeEachReceiveACK(t *testing.T) {
	b := celsiusTestBroker()
	primary, worker, client := newCelsiusFakeSocket(), newCelsiusFakeSocket(), newCelsiusFakeSocket()
	primary.onWrite = celsiusAutoAck(primary, false)
	worker.onWrite = celsiusAutoAck(worker, false)
	connected := make(chan struct{})
	b.Dial = func(ctx context.Context, endpoint string) (CelsiusSocket, error) {
		if strings.HasSuffix(endpoint, "primary") {
			return primary, nil
		}
		select {
		case <-connected:
			return worker, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	celsiusRunTest(t, b, client)
	client.push([]any{celsiusCommand(91, "subscribe", "calpico-room-live:worker-room"), celsiusCommand(92, "subscribe", "calpico-room-live:worker-room"), celsiusCommand(93, "unsubscribe", "calpico-room-live:worker-room"), celsiusCommand(99, "subscribe", "conversations")})
	celsiusAwaitReply(t, client, 99)
	close(connected)
	for _, id := range []int{91, 92, 93} {
		reply := celsiusAwaitReply(t, client, id)
		if reply["reply"].(map[string]any)["type"] == "error" {
			t.Fatalf("queued command %d failed", id)
		}
	}
	for _, kind := range []string{"subscribe", "subscribe", "unsubscribe"} {
		batch := celsiusReadBatch(t, worker)
		if batch[0]["command"].(map[string]any)["type"] != kind {
			t.Fatalf("wire ordering = %v", batch)
		}
	}
}

func TestCelsiusConnectPrecedesCompletedSharedSubscription(t *testing.T) {
	b := celsiusTestBroker()
	primary, worker, client := newCelsiusFakeSocket(), newCelsiusFakeSocket(), newCelsiusFakeSocket()
	primary.onWrite = celsiusAutoAck(primary, true)
	worker.onWrite = celsiusAutoAck(worker, false)
	connected := make(chan struct{})
	b.Dial = func(ctx context.Context, endpoint string) (CelsiusSocket, error) {
		if strings.HasSuffix(endpoint, "primary") {
			return primary, nil
		}
		select {
		case <-connected:
			return worker, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	celsiusRunTest(t, b, client)
	client.push([]any{celsiusCommand(91, "connect", ""), celsiusCommand(92, "subscribe", "calpico-chatgpt-messaging")})
	reply := celsiusAwaitReply(t, client, 92)["reply"].(map[string]any)
	if reply["recovered"] != false || len(reply["catchups"].([]any)) != 0 {
		t.Fatal("multi-owner recovery leaked a primary cursor")
	}
	close(connected)
	for _, kind := range []string{"connect", "subscribe"} {
		batch := celsiusReadBatch(t, worker)
		if batch[0]["command"].(map[string]any)["type"] != kind {
			t.Fatalf("wire ordering = %v", batch)
		}
	}
	primary.push([]any{map[string]any{"id": 1, "reply": map[string]any{"type": "connect"}}})
	celsiusAwaitReply(t, client, 91)
}

func TestCelsiusACKDeadlineStartsOnlyAfterSend(t *testing.T) {
	b := celsiusTestBroker()
	b.tickInterval = time.Millisecond
	var now atomic.Int64
	now.Store(time.Now().UnixNano())
	b.now = func() time.Time { return time.Unix(0, now.Load()) }
	primary, worker, client := newCelsiusFakeSocket(), newCelsiusFakeSocket(), newCelsiusFakeSocket()
	primary.onWrite = celsiusAutoAck(primary, false)
	worker.onWrite = celsiusAutoAck(worker, false)
	connected := make(chan struct{})
	b.Dial = func(ctx context.Context, endpoint string) (CelsiusSocket, error) {
		if strings.HasSuffix(endpoint, "primary") {
			return primary, nil
		}
		select {
		case <-connected:
			return worker, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	celsiusRunTest(t, b, client)
	client.push([]any{celsiusCommand(91, "subscribe", "calpico-room-live:worker-room"), celsiusCommand(99, "subscribe", "conversations")})
	celsiusAwaitReply(t, client, 99)
	now.Add(int64(21 * time.Second))
	client.push([]any{celsiusCommand(100, "subscribe", "conversations")})
	celsiusAwaitReply(t, client, 100)
	close(connected)
	reply := celsiusAwaitReply(t, client, 91)["reply"].(map[string]any)
	if reply["type"] != "subscribe" {
		t.Fatalf("setup consumed ACK deadline: %v", reply)
	}
}

func TestCelsiusSecondaryACKTimeoutKeepsPrimaryConnected(t *testing.T) {
	b := celsiusTestBroker()
	b.ackTimeout = 5 * time.Millisecond
	b.tickInterval = time.Millisecond
	b.retryInitial = time.Hour
	primary, worker, client := newCelsiusFakeSocket(), newCelsiusFakeSocket(), newCelsiusFakeSocket()
	primary.onWrite = celsiusAutoAck(primary, false)
	b.Dial = func(_ context.Context, endpoint string) (CelsiusSocket, error) {
		if strings.HasSuffix(endpoint, "primary") {
			return primary, nil
		}
		return worker, nil
	}
	celsiusRunTest(t, b, client)
	client.push([]any{celsiusCommand(91, "subscribe", "calpico-room-live:worker-room")})
	reply := celsiusAwaitReply(t, client, 91)["reply"].(map[string]any)
	if reply["code"] != "cq_owner_unavailable" {
		t.Fatalf("missing bounded failure: %v", reply)
	}
	client.push([]any{celsiusCommand(99, "subscribe", "conversations")})
	celsiusAwaitReply(t, client, 99)
	select {
	case <-primary.closed:
		t.Fatal("secondary timeout retired primary")
	default:
	}
}

type celsiusDelayedEOFSocket struct {
	*celsiusFakeSocket
	release chan struct{}
}

func (s *celsiusDelayedEOFSocket) ReadMessage() (int, []byte, error) {
	<-s.closed
	<-s.release
	return 0, nil, io.EOF
}

func TestCelsiusRetiredEOFAndReplayACKCannotAffectReplacement(t *testing.T) {
	b := celsiusTestBroker()
	b.ackTimeout, b.retryInitial, b.tickInterval = 5*time.Millisecond, time.Millisecond, time.Millisecond
	events := make(chan map[string]any, 64)
	b.Event = func(event map[string]any) { events <- event }
	primary, replacement, client := newCelsiusFakeSocket(), newCelsiusFakeSocket(), newCelsiusFakeSocket()
	retired := &celsiusDelayedEOFSocket{newCelsiusFakeSocket(), make(chan struct{})}
	primary.onWrite, replacement.onWrite = celsiusAutoAck(primary, false), celsiusAutoAck(replacement, false)
	var attempts atomic.Int64
	b.Dial = func(_ context.Context, endpoint string) (CelsiusSocket, error) {
		if strings.HasSuffix(endpoint, "primary") {
			return primary, nil
		}
		if attempts.Add(1) == 1 {
			return retired, nil
		}
		return replacement, nil
	}
	celsiusRunTest(t, b, client)
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(retired.release) }) })
	celsiusAwaitEvent(t, events, "celsius_owner_connected", "worker")
	client.push([]any{celsiusCommand(91, "subscribe", "calpico-room-live:worker-room")})
	if reply := celsiusAwaitReply(t, client, 91)["reply"].(map[string]any); reply["code"] != "cq_owner_unavailable" {
		t.Fatalf("retired command = %v", reply)
	}
	celsiusAwaitEvent(t, events, "celsius_owner_connected", "worker")
	celsiusReadBatch(t, replacement) // The completed command is replayed only as desired state.
	release.Do(func() { close(retired.release) })
	celsiusAwaitEvent(t, events, "celsius_connection_closed", true)
	client.push([]any{celsiusCommand(91, "subscribe", "calpico-room-live:worker-room"), celsiusCommand(99, "subscribe", "conversations")})
	seen := map[int64]int{}
	for len(seen) < 2 {
		for _, frame := range celsiusReadBatch(t, client) {
			id, valid := celsiusID(frame["id"])
			if !valid {
				t.Fatal("missing native response ID")
			}
			seen[id]++
			if reply := frame["reply"].(map[string]any); reply["type"] == "error" {
				t.Fatalf("replacement failed after retired EOF: %v", reply)
			}
		}
	}
	if seen[91] != 1 || seen[99] != 1 {
		t.Fatalf("duplicate or misrouted replies: %v", seen)
	}
	select {
	case <-replacement.closed:
		t.Fatal("retired EOF closed replacement")
	case data := <-client.writes:
		t.Fatalf("late replay ACK escaped: %s", data)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestCelsiusSocketKeepaliveUsesPongsAndClosesOnlyMissedPing(t *testing.T) {
	var drop atomic.Bool
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetPingHandler(func(value string) error {
			if drop.Load() {
				return nil
			}
			if err := conn.WriteControl(websocket.PongMessage, []byte("unsolicited"), time.Now().Add(time.Second)); err != nil {
				return err
			}
			return conn.WriteControl(websocket.PongMessage, []byte(value), time.Now().Add(time.Second))
		})
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failed := make(chan struct{})
	var once sync.Once
	socketKeepaliveAt(ctx, conn, func() { once.Do(func() { close(failed) }); conn.Close() }, 5*time.Millisecond, 30*time.Millisecond)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { recover() }()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
	select {
	case <-failed:
		t.Fatal("responsive idle peer expired")
	case <-time.After(65 * time.Millisecond):
	}
	drop.Store(true)
	select {
	case <-failed:
	case <-time.After(time.Second):
		t.Fatal("missed pong did not close connection")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("keepalive left reader blocked")
	}
}

type celsiusHeldPingWriter struct {
	net.Conn
	release <-chan struct{}
	once    sync.Once
}

func (c *celsiusHeldPingWriter) Write(data []byte) (int, error) {
	n, err := c.Conn.Write(data)
	if len(data) > 0 && data[0] == 0x89 {
		c.once.Do(func() { <-c.release })
	}
	return n, err
}

func TestCelsiusUnsolicitedPongCannotHideMatchingPong(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	dialer := websocket.Dialer{NetDialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &celsiusHeldPingWriter{Conn: conn, release: release}, nil
	}}
	conn, _, err := dialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failed, received := make(chan struct{}), make(chan struct{})
	var failureOnce, receivedOnce sync.Once
	socketKeepaliveAt(ctx, conn, func() { failureOnce.Do(func() { close(failed) }); conn.Close() }, 10*time.Millisecond, 20*time.Millisecond)
	handlePong := conn.PongHandler()
	conn.SetPongHandler(func(value string) error {
		err := handlePong(value)
		if value != "" {
			receivedOnce.Do(func() { close(received) })
		}
		return err
	})
	_ = handlePong("") // A peer may send an unsolicited pong between pings.
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { recover() }()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("matching pong was not read")
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-failed:
		t.Fatal("unsolicited pong hid the matching pong")
	case <-time.After(45 * time.Millisecond):
	}
	cancel()
	conn.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("keepalive reader did not stop")
	}
}
