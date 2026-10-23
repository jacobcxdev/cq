package dots

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type cloudTestWriter struct {
	write func([]byte) (int, error)
	close func() error
}

func (w cloudTestWriter) Write(raw []byte) (int, error) { return w.write(raw) }
func (w cloudTestWriter) Close() error {
	if w.close != nil {
		return w.close()
	}
	return nil
}

type cloudTestOutput struct {
	mu     sync.Mutex
	data   bytes.Buffer
	values chan cloudMessage
}

func (o *cloudTestOutput) Write(raw []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.data.Write(raw)
	var value cloudMessage
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, err
	}
	o.values <- value
	return len(raw), nil
}

func (o *cloudTestOutput) text() string { o.mu.Lock(); defer o.mu.Unlock(); return o.data.String() }

type cloudTestSocket struct {
	mu        sync.Mutex
	messages  []cloudMessage
	output    chan []byte
	closed    chan struct{}
	closeOnce sync.Once
	onSend    func(cloudMessage) (bool, error)
}

func newCloudTestSocket() *cloudTestSocket {
	return &cloudTestSocket{output: make(chan []byte, 128), closed: make(chan struct{})}
}
func (s *cloudTestSocket) ReadMessage() (int, []byte, error) {
	select {
	case raw := <-s.output:
		if raw == nil {
			return 0, nil, io.EOF
		}
		return websocket.TextMessage, raw, nil
	case <-s.closed:
		return 0, nil, io.EOF
	}
}
func (s *cloudTestSocket) WriteMessage(_ int, raw []byte) error {
	var request cloudMessage
	if err := json.Unmarshal(raw, &request); err != nil {
		return err
	}
	s.mu.Lock()
	s.messages = append(s.messages, request)
	onSend := s.onSend
	s.mu.Unlock()
	if onSend != nil {
		handled, err := onSend(request)
		if handled || err != nil {
			return err
		}
	}
	if id, ok := request["id"]; ok {
		var result any = map[string]any{}
		if request.method() == "thread/resume" {
			result = map[string]any{"thread": map[string]any{"id": cloudThreadID(request["params"])}}
		}
		response, _ := json.Marshal(map[string]any{"id": id, "result": result})
		s.output <- response
	}
	return nil
}
func (s *cloudTestSocket) SetReadDeadline(time.Time) error { return nil }
func (s *cloudTestSocket) Close() error                    { s.closeOnce.Do(func() { close(s.closed) }); return nil }
func (s *cloudTestSocket) snapshot() []cloudMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]cloudMessage(nil), s.messages...)
}

type cloudTestHarness struct {
	t           *testing.T
	cfg         *Settings
	accounts    []Account
	deps        cloudStdioDependencies
	input       *io.PipeWriter
	output      *cloudTestOutput
	finished    chan error
	localMu     sync.Mutex
	localRaw    [][]byte
	socketMu    sync.Mutex
	sockets     []*cloudTestSocket
	dialOwners  []Account
	eventsMu    sync.Mutex
	events      []map[string]any
	localOutput *io.PipeWriter
	localExit   chan error
}

func cloudTestToken() string {
	claims, _ := json.Marshal(map[string]any{"exp": time.Now().Add(time.Hour).Unix()})
	return "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".synthetic"
}

func newCloudTestHarness(t *testing.T) *cloudTestHarness {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	h := &cloudTestHarness{t: t, cfg: &Settings{RuntimeRoot: root, AppPath: "/synthetic/Codex.app", SystemAlias: "11111111", SystemAccountID: "primary-account", AppVersion: "test", Dots: map[string]Dot{
		"primary":   {OwnerAlias: "11111111", OwnerAccountID: "primary-account", ProfileID: "primary-profile", RootThreadID: "primary-root"},
		"secondary": {OwnerAlias: "22222222", OwnerAccountID: "secondary-account", ProfileID: "secondary-profile", RootThreadID: "secondary-root"},
	}}, accounts: []Account{{Alias: "11111111", AccountID: "primary-account", Token: cloudTestToken()}, {Alias: "22222222", AccountID: "secondary-account", Token: cloudTestToken()}},
		output: &cloudTestOutput{values: make(chan cloudMessage, 256)}, finished: make(chan error, 1), localExit: make(chan error, 1)}
	h.deps = cloudStdioDependencies{environment: []string{"UNRELATED=kept"}, readRoots: ReadOwned, now: time.Now, statusInterval: time.Hour,
		event: func(value map[string]any) { h.eventsMu.Lock(); h.events = append(h.events, value); h.eventsMu.Unlock() },
		stock: func(context.Context, string, []string, []string, io.Reader, io.Writer, io.Writer) error {
			return errors.New("unexpected stock execution")
		},
		launch: func(_ context.Context, _ string, _ []string, _ []string, _ io.Writer) (*cloudLocal, error) {
			output, writer := io.Pipe()
			h.localOutput = writer
			var stop sync.Once
			return &cloudLocal{output: output, input: cloudTestWriter{write: func(raw []byte) (int, error) {
				h.localMu.Lock()
				h.localRaw = append(h.localRaw, bytes.Clone(raw))
				h.localMu.Unlock()
				var request cloudMessage
				if err := json.Unmarshal(raw, &request); err != nil {
					return 0, err
				}
				if id, ok := request["id"]; ok && request.method() != "" {
					response, _ := json.Marshal(map[string]any{"id": id, "result": map[string]any{}})
					if _, err := writer.Write(append(response, '\n')); err != nil {
						return 0, err
					}
				}
				return len(raw), nil
			}}, wait: func() error { return <-h.localExit }, stop: func() {
				stop.Do(func() {
					_ = writer.Close()
					_ = output.Close()
					select {
					case h.localExit <- nil:
					default:
					}
				})
			}}, nil
		},
		dial: func(_ context.Context, owner Account) (cloudSocket, int, error) {
			socket := newCloudTestSocket()
			h.socketMu.Lock()
			h.sockets = append(h.sockets, socket)
			h.dialOwners = append(h.dialOwners, owner)
			h.socketMu.Unlock()
			return socket, 101, nil
		},
		http: cloudTestHTTP(func(r *http.Request) (*http.Response, error) {
			return cloudResponse(200, `{"thread":{"id":"secondary-root","status":{"type":"idle"}}}`), nil
		}),
	}
	return h
}

func (h *cloudTestHarness) start() {
	h.t.Helper()
	reader, writer := io.Pipe()
	h.input = writer
	go func() {
		h.finished <- runCloudStdio(context.Background(), h.cfg, h.accounts, []string{"app-server", "-c", cloudAppMarker + "true"}, reader, h.output, io.Discard, h.deps)
	}()
	h.t.Cleanup(func() {
		_ = writer.Close()
		select {
		case err := <-h.finished:
			if err != nil {
				h.t.Errorf("adapter exit: %v", err)
			}
		case <-time.After(3 * time.Second):
			h.t.Error("adapter did not stop")
		}
		_ = reader.Close()
	})
}

func (h *cloudTestHarness) send(id any, method, root string) {
	h.t.Helper()
	request := map[string]any{"method": method}
	if id != nil {
		request["id"] = id
	}
	if root != "" {
		request["params"] = map[string]any{"threadId": root}
	}
	raw, _ := json.Marshal(request)
	if _, err := h.input.Write(append(raw, '\n')); err != nil {
		h.t.Fatal(err)
	}
}

func (h *cloudTestHarness) await(id any) cloudMessage {
	h.t.Helper()
	return h.awaitWithin(id, 3*time.Second)
}

func (h *cloudTestHarness) awaitWithin(id any, timeout time.Duration) cloudMessage {
	h.t.Helper()
	expected, _ := json.Marshal(id)
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case value := <-h.output.values:
			if bytes.Equal(value["id"], expected) {
				return value
			}
		case <-deadline.C:
			h.t.Fatalf("response %s unavailable; output %s", expected, h.output.text())
			return nil
		}
	}
}

func (h *cloudTestHarness) initialize() {
	h.send(1, "initialize", "")
	h.await(1)
	h.send(nil, "initialized", "")
}
func (h *cloudTestHarness) localMessages() [][]byte {
	h.localMu.Lock()
	defer h.localMu.Unlock()
	return append([][]byte(nil), h.localRaw...)
}
func (h *cloudTestHarness) socketCount() int {
	h.socketMu.Lock()
	defer h.socketMu.Unlock()
	return len(h.sockets)
}

type cloudTestHTTP func(*http.Request) (*http.Response, error)

func (f cloudTestHTTP) Do(r *http.Request) (*http.Response, error) { return f(r) }
func cloudResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestCloudNativeArguments(t *testing.T) {
	arguments := []string{"app-server", "-c", `openai_base_url="https://old.invalid"`}
	actual, err := nativeCloudArguments(arguments, []string{"OPENAI_BASE_URL=http://127.0.0.1:19321"})
	if err != nil || len(actual) != len(arguments)+2 || actual[len(actual)-1] != `openai_base_url="http://127.0.0.1:19321"` {
		t.Fatalf("arguments: %v, %v", actual, err)
	}
	if len(arguments) != 3 {
		t.Fatal("mutated caller arguments")
	}
	for _, base := range []string{"", "https://127.0.0.1:1234", "http://localhost:1234", "http://127.0.0.1", "http://user@127.0.0.1:1234", "http://127.0.0.1:99999", "http://127.0.0.1:1234?token=bad", "http://127.0.0.1:1234/path"} {
		if _, err := nativeCloudArguments(arguments, []string{"OPENAI_BASE_URL=" + base}); err == nil {
			t.Errorf("accepted %q", base)
		}
	}
}

func TestCloudThreadIDShape(t *testing.T) {
	for _, test := range []struct{ value, expected string }{
		{`{"threadId":"x"}`, "x"}, {`{"thread":{"id":"nested"}}`, "nested"},
		{`{"threadId":7,"thread":{"id":"nested"}}`, "nested"}, {`{"thread":false}`, ""},
		{`{"threadId":"","thread":{"id":"nested"}}`, ""}, {`null`, ""},
	} {
		if actual := cloudThreadID([]byte(test.value)); actual != test.expected {
			t.Errorf("thread ID %s = %q, want %q", test.value, actual, test.expected)
		}
	}
}

func TestCloudUnmarkedAndNonServerKeepStockPath(t *testing.T) {
	for _, arguments := range [][]string{{"--version"}, {"app-server", "-c", `cli_auth_credentials_store="keyring"`}} {
		t.Run(arguments[0], func(t *testing.T) {
			h := newCloudTestHarness(t)
			h.deps.stock = func(_ context.Context, path string, actual, env []string, _ io.Reader, _, _ io.Writer) error {
				if path != filepath.Join(h.cfg.AppPath, "Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex") || !reflectCloudEqual(actual, arguments) {
					t.Errorf("stock path/argv changed: %s %v", path, actual)
				}
				if environmentValue(env, "CODEX_CLI_PATH") != path || environmentValue(env, "UNRELATED") != "kept" {
					t.Error("stock environment changed")
				}
				return nil
			}
			if err := runCloudStdio(context.Background(), h.cfg, nil, arguments, strings.NewReader(""), io.Discard, io.Discard, h.deps); err != nil {
				t.Fatal(err)
			}
			if h.socketCount() != 0 {
				t.Fatal("stock invocation opened cloud session")
			}
		})
	}
}

func reflectCloudEqual(left, right []string) bool {
	return strings.Join(left, "\x00") == strings.Join(right, "\x00")
}

func TestCloudStockRPCsAndAuthenticationRemainUnchanged(t *testing.T) {
	h := newCloudTestHarness(t)
	h.start()
	h.initialize()
	raw := []byte("{ \"id\":\"cq-internal-user\", \"method\":\"future/stockOperation\",\"params\":{\"data\":\"" + strings.Repeat("x", 4*1024*1024) + "\"}}\n")
	if _, err := h.input.Write(raw); err != nil {
		t.Fatal(err)
	}
	// This byte-preservation check parses 4 MiB in both the adapter and fake
	// stock child. Allow race-instrumented work under contention; latency is
	// checked separately by the responsiveness tests.
	h.awaitWithin("cq-internal-user", 10*time.Second)
	h.send(2, "account/read", "")
	h.await(2)
	callback := []byte(`{"id":"refresh-request","method":"account/chatgptAuthTokens/refresh","params":{"reason":"unauthorized"}}` + "\n")
	if _, err := h.localOutput.Write(callback); err != nil {
		t.Fatal(err)
	}
	if value := h.await("refresh-request"); value.method() != "account/chatgptAuthTokens/refresh" {
		t.Fatal("stock callback changed")
	}
	reply := []byte(`{"id":"refresh-request","result":{"accessToken":"synthetic-refreshed"}}` + "\n")
	if _, err := h.input.Write(reply); err != nil {
		t.Fatal(err)
	}
	h.send(3, "turn/start", "ordinary-task")
	h.await(3)
	local := h.localMessages()
	if len(local) != 6 || !bytes.Equal(local[2], raw) || !bytes.Equal(local[4], reply) {
		t.Fatalf("stock requests changed: %d messages", len(local))
	}
	if h.socketCount() != 0 {
		t.Fatal("ordinary task opened cloud connection")
	}
}

func TestCloudPinnedRootsAndSecondaryMutationBoundary(t *testing.T) {
	h := newCloudTestHarness(t)
	h.start()
	h.initialize()
	h.send(2, "thread/resume", "primary-root")
	h.await(2)
	h.send(3, "thread/resume", "secondary-root")
	h.await(3)
	h.send(4, "turn/start", "secondary-root")
	if response := h.await(4); response["error"] == nil {
		t.Fatal("secondary write allowed")
	}
	h.send(5, "turn/start", "primary-root")
	h.await(5)
	h.send(6, "thread/resume", "ordinary-task")
	h.await(6)
	if h.socketCount() != 2 || len(h.localMessages()) != 4 {
		t.Fatalf("routing changed: sockets=%d local=%d", h.socketCount(), len(h.localMessages()))
	}
	h.socketMu.Lock()
	defer h.socketMu.Unlock()
	for index, owner := range h.dialOwners {
		if owner.Alias != h.accounts[index].Alias || owner.AccountID != h.accounts[index].AccountID {
			t.Error("owner identity changed")
		}
	}
}

func TestCloudMissingInventoryPreservesLocalAndFailsDot(t *testing.T) {
	h := newCloudTestHarness(t)
	h.accounts = nil
	h.start()
	h.initialize()
	h.send(2, "thread/resume", "secondary-root")
	if h.await(2)["error"] == nil {
		t.Fatal("missing owner allowed")
	}
	h.send(3, "thread/start", "")
	h.await(3)
	if h.socketCount() != 0 || len(h.localMessages()) != 3 {
		t.Fatal("missing owner disrupted stock path")
	}
}

func TestCloudOwnerConnectDoesNotBlockPrimaryAndQueueIsBounded(t *testing.T) {
	h := newCloudTestHarness(t)
	started, release := make(chan struct{}), make(chan struct{})
	original := h.deps.dial
	h.deps.dial = func(ctx context.Context, owner Account) (cloudSocket, int, error) {
		close(started)
		select {
		case <-release:
			return original(ctx, owner)
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		}
	}
	h.start()
	h.initialize()
	h.send(2, "thread/resume", "secondary-root")
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("connection did not start")
	}
	for id := 3; id <= 35; id++ {
		h.send(id, "thread/resume", "secondary-root")
	}
	if h.await(35)["error"] == nil {
		t.Fatal("queue overflow accepted")
	}
	h.send(36, "turn/start", "ordinary-task")
	if h.await(36)["error"] != nil {
		t.Fatal("primary blocked")
	}
	close(release)
	for id := 2; id <= 34; id++ {
		if h.await(id)["error"] != nil {
			t.Fatalf("owner request %d failed", id)
		}
	}
	if h.socketCount() != 1 {
		t.Fatal("duplicated owner session")
	}
}

func TestCloudOwnerEOFFailsOnceAndLaterRequestReconnects(t *testing.T) {
	h := newCloudTestHarness(t)
	original := h.deps.dial
	h.deps.dial = func(ctx context.Context, owner Account) (cloudSocket, int, error) {
		connection, code, err := original(ctx, owner)
		if h.socketCount() == 1 {
			connection.(*cloudTestSocket).onSend = func(request cloudMessage) (bool, error) {
				if request.method() == "thread/resume" {
					connection.(*cloudTestSocket).output <- nil
					return true, nil
				}
				return false, nil
			}
		}
		return connection, code, err
	}
	h.start()
	h.initialize()
	h.send(18, "thread/resume", "secondary-root")
	response := h.await(18)
	if string(response["id"]) != "18" || !bytes.Contains(response["error"], []byte("closed before responding")) {
		t.Fatalf("lost request: %s", response["error"])
	}
	h.send(19, "thread/resume", "secondary-root")
	if h.await(19)["error"] != nil {
		t.Fatal("replacement did not answer")
	}
	if h.socketCount() != 2 {
		t.Fatal("dead owner socket reused")
	}
	if strings.Count(h.output.text(), `"id":18`) != 1 {
		t.Fatal("lost request failed more than once")
	}
	for _, socket := range h.sockets {
		if len(socket.snapshot()) != 3 {
			t.Fatal("old read replayed")
		}
	}
}

func TestCloudResponseArrivingDuringSendFailureIsDeliveredOnce(t *testing.T) {
	h := newCloudTestHarness(t)
	original := h.deps.dial
	answered := make(chan struct{})
	h.deps.dial = func(ctx context.Context, owner Account) (cloudSocket, int, error) {
		connection, code, err := original(ctx, owner)
		socket := connection.(*cloudTestSocket)
		socket.onSend = func(request cloudMessage) (bool, error) {
			if request.method() != "thread/resume" {
				return false, nil
			}
			raw, _ := json.Marshal(map[string]any{"id": request["id"], "result": map[string]any{"thread": map[string]any{"id": "secondary-root"}}})
			socket.output <- raw
			select {
			case <-answered:
			case <-ctx.Done():
			}
			return true, errors.New("late synthetic write failure")
		}
		return connection, code, err
	}
	h.start()
	h.initialize()
	h.send(18, "thread/resume", "secondary-root")
	if h.await(18)["error"] != nil {
		t.Fatal("response lost to write failure")
	}
	close(answered)
	h.send(19, "thread/start", "")
	h.await(19)
	if strings.Count(h.output.text(), `"id":18`) != 1 {
		t.Fatal("request completed twice")
	}
}

func TestCloudEOFArrivingDuringSendFailureIsDeliveredOnce(t *testing.T) {
	h := newCloudTestHarness(t)
	original := h.deps.dial
	answered := make(chan struct{})
	h.deps.dial = func(ctx context.Context, owner Account) (cloudSocket, int, error) {
		connection, code, err := original(ctx, owner)
		if h.socketCount() != 1 {
			return connection, code, err
		}
		socket := connection.(*cloudTestSocket)
		socket.onSend = func(request cloudMessage) (bool, error) {
			if request.method() != "thread/resume" {
				return false, nil
			}
			socket.output <- nil
			select {
			case <-answered:
			case <-ctx.Done():
			}
			return true, errors.New("late synthetic write failure")
		}
		return connection, code, err
	}
	h.start()
	h.initialize()
	h.send(18, "thread/resume", "secondary-root")
	if h.await(18)["error"] == nil {
		t.Fatal("EOF did not fail outstanding read")
	}
	close(answered)
	h.send(19, "thread/resume", "secondary-root")
	if h.await(19)["error"] != nil {
		t.Fatal("replacement owner did not answer")
	}
	if strings.Count(h.output.text(), `"id":18`) != 1 {
		t.Fatal("EOF and send failure completed request twice")
	}
}

func TestCloudNativeRootSnapshotValidationAndHistory(t *testing.T) {
	h := newCloudTestHarness(t)
	b := &cloudBridge{cfg: h.cfg, deps: h.deps, roots: map[string]string{"old-root": "22222222"}}
	snapshot := `{"version":1,"roots":[{"owner_alias":"11111111","profile_id":"primary-profile","root_thread_id":"fresh-primary"},{"owner_alias":"22222222","profile_id":"secondary-profile","root_thread_id":"fresh-secondary"}]}`
	publish := func(data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(h.cfg.RuntimeRoot, "native-roots.json"), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, invalid := range []string{strings.Replace(snapshot, `"version":1`, `"version":true`, 1), strings.Replace(snapshot, "secondary-profile", "wrong-profile", 1), strings.Replace(snapshot, "fresh-secondary", "fresh-primary", 1), strings.Replace(snapshot, `"version":1`, `"extra":0,"version":1`, 1), `{"version":1,"roots":[]}`} {
		publish(invalid)
		if b.threadOwner("fresh-primary") != "" || len(b.roots) != 1 {
			t.Fatalf("invalid snapshot partially accepted: %s", invalid)
		}
	}
	publish(snapshot)
	if b.threadOwner("fresh-secondary") != "22222222" || b.threadOwner("old-root") != "22222222" || b.threadOwner("unpublished") != "" {
		t.Fatal("root rotation lost verified ownership")
	}
}

func TestCloudRootSnapshotDoesNotDelayOrdinaryRequests(t *testing.T) {
	h := newCloudTestHarness(t)
	h.start()
	h.initialize()
	h.send(2, "thread/resume", "ordinary-task")
	h.await(2)
	snapshot := `{"version":1,"roots":[{"owner_alias":"11111111","profile_id":"primary-profile","root_thread_id":"fresh-primary"},{"owner_alias":"22222222","profile_id":"secondary-profile","root_thread_id":"fresh-secondary"}]}`
	if err := os.WriteFile(filepath.Join(h.cfg.RuntimeRoot, "native-roots.json"), []byte(snapshot), 0o600); err != nil {
		t.Fatal(err)
	}
	h.send(3, "thread/resume", "fresh-secondary")
	h.await(3)
	if len(h.localMessages()) != 3 || h.socketCount() != 1 {
		t.Fatal("published root failed routing")
	}
}

func TestCloudHTTPReadAndStatusUsePinnedOwner(t *testing.T) {
	h := newCloudTestHarness(t)
	var mu sync.Mutex
	var requests []*http.Request
	h.deps.http = cloudTestHTTP(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		requests = append(requests, request)
		mu.Unlock()
		if request.Header.Get("ChatGPT-Account-Id") != "secondary-account" || request.Header.Get("Authorization") != "Bearer "+h.accounts[1].Token || request.Header.Get("X-OpenAI-Product-Sku") != "aeon" {
			t.Error("HTTP owner headers changed")
		}
		return cloudResponse(200, `{"thread":{"id":"secondary-root","turns":[{"id":"old"}],"status":{"type":"active","activeFlags":["waitingOnApproval"]}}}`), nil
	})
	h.start()
	h.initialize()
	h.send(2, "thread/read", "secondary-root")
	response := h.await(2)
	if !bytes.Contains(response["result"], []byte(`"turns":[]`)) {
		t.Fatalf("read turns retained: %s", response["result"])
	}
	h.send(3, "thread/resume", "secondary-root")
	h.await(3)
	select {
	case notification := <-h.output.values:
		if notification.method() != "thread/status/changed" {
			t.Fatalf("status notification %s", notification.method())
		}
	case <-time.After(time.Second):
		t.Fatal("owner status was not refreshed")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 || requests[0].URL.Path != "/v1/threads/secondary-root" {
		t.Fatalf("HTTP reads: %d", len(requests))
	}
}

func TestCloudHTTPInvalidResponsesFailClosed(t *testing.T) {
	for _, test := range []struct {
		name, body string
		status     int
	}{{"mismatch", `{"thread":{"id":"other"}}`, 200}, {"invalid", `not-json`, 200}, {"unauthorized", `{}`, 401}, {"bound", strings.Repeat("x", 16*1024*1024+1), 200}} {
		t.Run(test.name, func(t *testing.T) {
			h := newCloudTestHarness(t)
			h.deps.http = cloudTestHTTP(func(*http.Request) (*http.Response, error) { return cloudResponse(test.status, test.body), nil })
			h.start()
			h.initialize()
			h.send(2, "thread/read", "secondary-root")
			if h.await(2)["error"] == nil {
				t.Fatal("invalid HTTP response accepted")
			}
		})
	}
}

func TestCloudStatusValidation(t *testing.T) {
	h := newCloudTestHarness(t)
	b := &cloudBridge{ctx: context.Background(), cfg: h.cfg, deps: h.deps, owners: map[string]Account{h.accounts[1].Alias: h.accounts[1]}, roots: map[string]string{"secondary-root": "22222222"}}
	for _, body := range []string{`{"thread":{"id":"other","status":{"type":"idle"}}}`, `{"thread":{"id":"secondary-root","status":{"type":"wrong"}}}`, `{"thread":{"id":"secondary-root","status":{"type":"active","activeFlags":null}}}`, `{"thread":{"id":"secondary-root","status":{"type":"active","activeFlags":[1]}}}`, `{"thread":{"id":"secondary-root","status":{"type":"active","activeFlags":[null]}}}`} {
		b.deps.http = cloudTestHTTP(func(*http.Request) (*http.Response, error) { return cloudResponse(200, body), nil })
		if _, err := b.ownerThreadStatus("22222222", "secondary-root"); err == nil {
			t.Errorf("accepted invalid status %s", body)
		}
	}
	if _, err := b.ownerThreadStatus("11111111", "secondary-root"); err == nil {
		t.Fatal("accepted wrong owner")
	}
	if !sameCloudStatus(json.RawMessage(`{"type":"active","activeFlags":[]}`), json.RawMessage(`{"activeFlags":[],"type":"active"}`)) {
		t.Fatal("status key order created change")
	}
}

func TestCloudOwnerPanicFailsRequestAndKeepsWorker(t *testing.T) {
	h := newCloudTestHarness(t)
	original := h.deps.dial
	first := true
	h.deps.dial = func(ctx context.Context, owner Account) (cloudSocket, int, error) {
		if first {
			first = false
			panic("synthetic-sensitive-error")
		}
		return original(ctx, owner)
	}
	h.start()
	h.initialize()
	h.send(2, "thread/resume", "secondary-root")
	if h.await(2)["error"] == nil {
		t.Fatal("owner panic left request pending")
	}
	h.send(3, "thread/resume", "secondary-root")
	if h.await(3)["error"] != nil {
		t.Fatal("owner worker did not survive panic")
	}
	if strings.Contains(h.output.text(), "synthetic-sensitive-error") {
		t.Fatal("panic contents leaked")
	}
}

func TestCloudHelperProcess(t *testing.T) {
	if os.Getenv("CQ_CLOUD_TEST_CHILD") != "1" {
		return
	}
	var value cloudMessage
	if json.NewDecoder(os.Stdin).Decode(&value) != nil {
		os.Exit(8)
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"id": value["id"], "result": map[string]any{"data": strings.Repeat("x", 256*1024)}})
	os.Exit(7)
}

func TestCloudRealChildExitDrainsOutputWithOpenClientPipe(t *testing.T) {
	h := newCloudTestHarness(t)
	h.deps.launch = func(ctx context.Context, _ string, _ []string, env []string, stderr io.Writer) (*cloudLocal, error) {
		return startCloudLocal(ctx, os.Args[0], []string{"-test.run=^TestCloudHelperProcess$"}, append(env, "CQ_CLOUD_TEST_CHILD=1"), stderr)
	}
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	go func() {
		h.finished <- runCloudStdio(context.Background(), h.cfg, h.accounts, []string{"app-server", "-c", cloudAppMarker + "true"}, reader, h.output, io.Discard, h.deps)
	}()
	h.input = writer
	h.send(1, "initialize", "")
	response := h.await(1)
	if len(response["result"]) < 256*1024 {
		t.Fatal("stock output truncated before child wait")
	}
	select {
	case err := <-h.finished:
		var exited *CloudStdioExitError
		if !errors.As(err, &exited) || exited.Code != 7 {
			t.Fatalf("child exit: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("real child exit waited on client input")
	}
}

func TestCloudResourceMismatchAndNotificationOwnerBoundary(t *testing.T) {
	h := newCloudTestHarness(t)
	original := h.deps.dial
	h.deps.dial = func(ctx context.Context, owner Account) (cloudSocket, int, error) {
		connection, code, err := original(ctx, owner)
		connection.(*cloudTestSocket).onSend = func(request cloudMessage) (bool, error) {
			if request.method() != "thread/resume" {
				return false, nil
			}
			response, _ := json.Marshal(map[string]any{"id": request["id"], "result": map[string]any{"thread": map[string]any{"id": "wrong-root"}}})
			connection.(*cloudTestSocket).output <- response
			return true, nil
		}
		return connection, code, err
	}
	h.start()
	h.initialize()
	h.send(2, "thread/resume", "secondary-root")
	if h.await(2)["error"] == nil {
		t.Fatal("resource mismatch accepted")
	}
	h.socketMu.Lock()
	socket := h.sockets[0]
	h.socketMu.Unlock()
	socket.output <- []byte(`{"method":"thread/status/changed","params":{"threadId":"primary-root","status":{"type":"idle"}}}`)
	socket.output <- []byte(`{"method":"thread/status/changed","params":{"threadId":"secondary-root","status":{"type":"idle"}}}`)
	select {
	case value := <-h.output.values:
		if cloudThreadID(value["params"]) != "secondary-root" {
			t.Fatal("foreign notification forwarded")
		}
	case <-time.After(time.Second):
		t.Fatal("owned notification lost")
	}
}

func TestCloudLocalExitReturnsEvenWithOpenInput(t *testing.T) {
	h := newCloudTestHarness(t)
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	go func() {
		h.finished <- runCloudStdio(context.Background(), h.cfg, h.accounts, []string{"app-server", "-c", cloudAppMarker + "true"}, reader, h.output, io.Discard, h.deps)
	}()
	h.input = writer
	h.send(1, "initialize", "")
	h.await(1)
	h.localExit <- &CloudStdioExitError{Code: 7}
	_ = h.localOutput.Close()
	select {
	case err := <-h.finished:
		var exited *CloudStdioExitError
		if !errors.As(err, &exited) || exited.Code != 7 {
			t.Fatalf("child exit: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("child exit waited on stdin")
	}
}

func TestCloudDiagnosticsDoNotPersistCredentials(t *testing.T) {
	h := newCloudTestHarness(t)
	path := filepath.Join(h.cfg.RuntimeRoot, "cloud-stdio-events.jsonl")
	if err := os.WriteFile(path, []byte("legacy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	h.deps.event = cloudEventSink(h.cfg.RuntimeRoot)
	h.start()
	h.initialize()
	h.send(2, "thread/resume", "secondary-root")
	h.await(2)
	data, err := os.ReadFile(filepath.Join(h.cfg.RuntimeRoot, "cloud-stdio-events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range h.accounts {
		if bytes.Contains(data, []byte(account.Token)) {
			t.Fatal("credential written to events")
		}
	}
	if !strings.Contains(string(data), `"initialize_success":true`) {
		t.Fatalf("events missing success: %s", data)
	}
	info, err := os.Stat(filepath.Join(h.cfg.RuntimeRoot, "cloud-stdio-events.jsonl"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("events permissions changed")
	}
}
