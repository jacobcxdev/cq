package dots

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/harness"
	"github.com/jacobcxdev/cq/internal/httputil"
)

// These readers keep native replies flowing while the harness waits for private
// RPCs. Every ordinary request retains its own response channel and ID space.
type installedGatewayInbox struct {
	t       *testing.T
	mu      sync.Mutex
	replies map[string]chan cloudMessage
	events  chan cloudMessage
	failed  chan error
	next    int
}

func newInstalledGatewayInbox(t *testing.T) *installedGatewayInbox {
	return &installedGatewayInbox{t: t, replies: make(map[string]chan cloudMessage), events: make(chan cloudMessage, 512), failed: make(chan error, 1)}
}
func (r *installedGatewayInbox) register() (int, chan cloudMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	ch := make(chan cloudMessage, 1)
	r.replies[fmt.Sprint(r.next)] = ch
	return r.next, ch
}
func (r *installedGatewayInbox) observe(message cloudMessage) {
	r.mu.Lock()
	ch := r.replies[cloudHarnessID(message["id"])]
	if ch != nil {
		delete(r.replies, cloudHarnessID(message["id"]))
	}
	r.mu.Unlock()
	if ch != nil {
		ch <- message
	} else if message.method() != "" {
		r.events <- message
	}
}
func (r *installedGatewayInbox) fail(err error) {
	select {
	case r.failed <- err:
	default:
	}
}
func (r *installedGatewayInbox) await(method string, ch chan cloudMessage) cloudMessage {
	r.t.Helper()
	select {
	case v := <-ch:
		if len(v["error"]) > 0 && string(v["error"]) != "null" {
			r.t.Fatalf("native %s rejected: %s", method, v["error"])
		}
		return cloudHarnessObject(v["result"])
	case err := <-r.failed:
		r.t.Fatalf("native %s transport: %v", method, err)
	case <-time.After(20 * time.Second):
		r.t.Fatalf("native %s timed out", method)
	}
	return nil
}
func (r *installedGatewayInbox) turnCompleted(id string) {
	r.t.Helper()
	for {
		select {
		case message := <-r.events:
			if message.method() == "turn/completed" && cloudThreadID(message["params"]) == id {
				if status := cloudHarnessString(cloudHarnessObject(cloudHarnessObject(message["params"])["turn"]), "status"); status != "completed" {
					r.t.Fatalf("native turn %s ended with status %s", id, status)
				}
				return
			}
		case err := <-r.failed:
			r.t.Fatal(err)
		case <-time.After(20 * time.Second):
			r.t.Fatalf("native turn %s did not complete", id)
		}
	}
}

type installedGatewayDirect struct {
	*installedGatewayInbox
	h *cloudHarness
	c *websocket.Conn
}

func newInstalledGatewayDirect(t *testing.T, h *cloudHarness, c *websocket.Conn) *installedGatewayDirect {
	r := &installedGatewayDirect{installedGatewayInbox: newInstalledGatewayInbox(t), h: h, c: c}
	h.b.local = &cloudLocal{input: cloudTestWriter{write: func(raw []byte) (int, error) { err := c.WriteMessage(websocket.TextMessage, raw); return len(raw), err }}}
	go func() {
		defer func() {
			if v := recover(); v != nil {
				r.fail(fmt.Errorf("native direct reader panic: %v", v))
			}
		}()
		for {
			_, raw, err := c.ReadMessage()
			if err != nil {
				r.fail(err)
				return
			}
			if h.observeFrom("stdio", raw) {
				continue
			}
			var message cloudMessage
			if json.Unmarshal(raw, &message) != nil {
				r.fail(errors.New("invalid native direct message"))
				return
			}
			r.observe(message)
		}
	}()
	return r
}
func (r *installedGatewayDirect) send(message map[string]any) {
	r.t.Helper()
	raw := cloudHarnessRaw(message)
	var request cloudMessage
	json.Unmarshal(raw, &request)
	prepared, err := r.h.prepare(request, raw)
	if err != nil {
		r.t.Fatalf("harness prepare %s: %v", request.method(), err)
	}
	r.h.b.localWrite.Lock()
	_, err = r.h.b.local.input.Write(prepared)
	r.h.b.localWrite.Unlock()
	if err != nil {
		r.t.Fatal(err)
	}
}
func (r *installedGatewayDirect) call(method string, params any) cloudMessage {
	r.t.Helper()
	id, ch := r.register()
	r.send(map[string]any{"id": id, "method": method, "params": params})
	return r.await(method, ch)
}

type installedGatewayBackend struct {
	*installedGatewayInbox
	c              *websocket.Conn
	write          sync.Mutex
	frames         *json.Encoder
	client, stream string
	inputSeq       int
	lastServer     atomic.Uint64
	lastAck        atomic.Uint64
	chunks         atomic.Int64
	chunkedReplies atomic.Int64
	chunkedBytes   atomic.Int64
	pongs          atomic.Int64
	holdPong       atomic.Bool
	held           chan json.RawMessage
	replay         json.RawMessage
}

func newInstalledGatewayBackend(t *testing.T, c *websocket.Conn, frames *json.Encoder) *installedGatewayBackend {
	r := &installedGatewayBackend{installedGatewayInbox: newInstalledGatewayInbox(t), c: c, frames: frames, client: "fixture-background-client", stream: "11111111-1111-4111-8111-111111111111", held: make(chan json.RawMessage, 1)}
	r.read()
	return r
}
func (r *installedGatewayBackend) writeEnvelope(envelope map[string]any) error {
	r.write.Lock()
	defer r.write.Unlock()
	r.frames.Encode(map[string]any{"direction": "backend-to-cq", "envelope": envelope})
	return r.c.WriteJSON(envelope)
}
func (r *installedGatewayBackend) read() {
	go func() {
		var chunkRaw []byte
		var chunkSeq uint64
		var chunkNext, chunkCount, chunkSize int
		defer func() {
			if v := recover(); v != nil {
				r.fail(fmt.Errorf("native gateway reader panic: %v", v))
			}
		}()
		for {
			var raw json.RawMessage
			if err := r.c.ReadJSON(&raw); err != nil {
				r.fail(err)
				return
			}
			r.write.Lock()
			r.frames.Encode(map[string]any{"direction": "cq-to-backend", "envelope": raw})
			r.write.Unlock()
			var e struct {
				Type         string       `json:"type"`
				Seq          uint64       `json:"seq_id"`
				Message      cloudMessage `json:"message"`
				SegmentID    int          `json:"segment_id"`
				SegmentCount int          `json:"segment_count"`
				MessageSize  int          `json:"message_size_bytes"`
				Chunk        string       `json:"message_chunk_base64"`
			}
			if json.Unmarshal(raw, &e) != nil {
				r.fail(errors.New("invalid native gateway envelope"))
				return
			}
			if e.Type == "ack" {
				continue
			}
			acknowledged := false
			messageRaw := raw
			if e.Type == "server_message_chunk" {
				// The production gateway leaves chunks opaque. Only this synthetic
				// backend assembles the native response and ACKs each wire segment.
				if _, interpreted := decodeCloudGatewayEnvelope(raw); interpreted {
					r.fail(errors.New("production codec interpreted a native response chunk"))
					return
				}
				if e.SegmentID == 0 {
					if len(chunkRaw) != 0 || e.Seq != r.lastServer.Load()+1 || e.SegmentCount < 2 || e.MessageSize <= 150*1024 || e.MessageSize > 1<<20 {
						r.fail(errors.New("unexpected native chunked response boundary"))
						return
					}
					chunkSeq, chunkNext, chunkCount, chunkSize = e.Seq, 0, e.SegmentCount, e.MessageSize
					r.lastServer.Store(e.Seq)
				}
				chunk, err := base64.StdEncoding.DecodeString(e.Chunk)
				if err != nil || e.Seq != chunkSeq || e.SegmentID != chunkNext || e.SegmentCount != chunkCount || e.MessageSize != chunkSize || len(chunkRaw)+len(chunk) > chunkSize {
					r.fail(errors.New("native response chunk lost ordering, bytes or metadata"))
					return
				}
				chunkRaw = append(chunkRaw, chunk...)
				chunkNext++
				r.chunks.Add(1)
				if err := r.writeEnvelope(map[string]any{"type": "ack", "client_id": r.client, "stream_id": r.stream, "seq_id": e.Seq, "segment_id": e.SegmentID, "cursor": fmt.Sprintf("fixture-ack-%d-%d", e.Seq, e.SegmentID)}); err != nil {
					r.fail(err)
					return
				}
				r.lastAck.Store(e.Seq)
				if chunkNext < chunkCount {
					continue
				}
				if len(chunkRaw) != chunkSize || json.Unmarshal(chunkRaw, &e.Message) != nil {
					r.fail(errors.New("native response chunk assembly did not match original message size"))
					return
				}
				r.chunkedReplies.Add(1)
				r.chunkedBytes.Store(int64(chunkSize))
				messageRaw = append(json.RawMessage(nil), chunkRaw...)
				chunkRaw = nil
				e.Type, acknowledged = "server_message", true
			} else if len(r.replay) > 0 {
				if string(raw) != string(r.replay) || e.Seq != r.lastServer.Load() {
					r.fail(errors.New("gateway private replay changed"))
					return
				}
				r.replay = nil
			} else {
				if e.Seq != r.lastServer.Load()+1 {
					r.fail(fmt.Errorf("gateway sequence gap last=%d next=%d", r.lastServer.Load(), e.Seq))
					return
				}
				r.lastServer.Store(e.Seq)
			}
			if e.Type == "pong" {
				r.pongs.Add(1)
				if r.holdPong.CompareAndSwap(true, false) {
					r.held <- append(json.RawMessage(nil), raw...)
					return
				}
			}
			if !acknowledged {
				if err := r.writeEnvelope(map[string]any{"type": "ack", "client_id": r.client, "stream_id": r.stream, "seq_id": e.Seq, "cursor": fmt.Sprintf("fixture-ack-%d", e.Seq)}); err != nil {
					r.fail(err)
					return
				}
				r.lastAck.Store(e.Seq)
			}
			if e.Type == "server_message" {
				if strings.Contains(string(messageRaw), "cq-harness/") {
					r.fail(errors.New("private native response escaped gateway"))
					return
				}
				r.observe(e.Message)
			}
		}
	}()
}
func (r *installedGatewayBackend) send(message map[string]any) {
	r.t.Helper()
	r.inputSeq++
	if err := r.writeEnvelope(map[string]any{"type": "client_message", "client_id": r.client, "stream_id": r.stream, "seq_id": r.inputSeq, "cursor": fmt.Sprintf("fixture-input-%d", r.inputSeq), "message": message}); err != nil {
		r.t.Fatal(err)
	}
}
func (r *installedGatewayBackend) call(method string, params any) cloudMessage {
	r.t.Helper()
	id, ch := r.register()
	r.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	return r.await(method, ch)
}
func TestHarnessInstalledGatewayReload(t *testing.T) {
	if os.Getenv("CQ_DOTS_HARNESS_INSTALLED_ACCEPTANCE") != "1" {
		t.Skip("opt-in native fixture")
	}
	if runtime.GOOS != "darwin" {
		t.Skip("loopback sandbox")
	}
	if !filepath.IsAbs(os.Getenv("CQ_DOTS_HARNESS_INSTALLED_CODEX")) {
		t.Fatal("installed native executable must be absolute")
	}
	for _, codeMode := range []bool{false, true} {
		t.Run(fmt.Sprintf("code-mode-%t", codeMode), func(t *testing.T) {
			artifacts := os.Getenv("CQ_DOTS_HARNESS_INSTALLED_ARTIFACTS")
			if artifacts == "" {
				artifacts = "/private/tmp"
			}
			root, err := os.MkdirTemp(artifacts, "cq-native-gateway-")
			if err != nil {
				t.Fatal(err)
			}
			if os.Getenv("CQ_DOTS_HARNESS_INSTALLED_ARTIFACTS") == "" {
				defer os.RemoveAll(root)
			}
			t.Logf("fixture-root=%s", root)
			for _, name := range []string{"home", "codex", "work", "tmp", "cache", "config", "data"} {
				if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
					t.Fatal(err)
				}
			}
			write := func(name, text string) {
				path := filepath.Join(root, name)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			write("codex/skills/qualification/SKILL.md", "---\nname: qualification\ndescription: CQ_QUALIFICATION_SKILL_MARKER\n---\nSynthetic isolated capability.\n")
			ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
			defer cancel()
			observed := make(chan map[string]any, 64)
			childObserved := make(chan map[string]any, 1)
			siblingObserved := make(chan map[string]any, 1)
			childRelease, siblingRelease := make(chan struct{}), make(chan struct{})
			var childOnce, siblingOnce sync.Once
			defer childOnce.Do(func() { close(childRelease) })
			defer siblingOnce.Do(func() { close(siblingRelease) })
			var spawnIssued, childHoldIssued, siblingHoldIssued atomic.Bool
			var parentThread atomic.Value
			parentThread.Store("")
			var policyEnabled atomic.Bool
			var siblingPolicy atomic.Value
			siblingPolicy.Store("")
			bridge := &cloudBridge{ctx: ctx, deps: cloudStdioDependencies{harnessTimeout: 8 * time.Second, harnessReceipts: harness.Receipts{FS: fsutil.OSFileSystem{}, Dir: filepath.Join(root, "receipts")}, readHarnessPolicy: func() (harness.Policy, error) {
				chats := map[string]harness.Profile{}
				if sibling := siblingPolicy.Load().(string); sibling != "" {
					chats[sibling] = harness.Full
				}
				return harness.Policy{Version: 1, Enabled: policyEnabled.Load(), Chats: chats}, nil
			}}}
			h := newCloudHarness(bridge)
			gateway := newCloudHarnessGateway(h)
			h.gateway = gateway
			gatewayConnected := make(chan *websocket.Conn, 4)
			gatewayCursors := make(chan string, 4)
			upgrader := websocket.Upgrader{}
			jwt := "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"fixture-user","email":"fixture@example.invalid","exp":4102444800,"https://api.openai.com/auth":{"chatgpt_account_id":"fixture-account","chatgpt_user_id":"fixture-user","chatgpt_plan_type":"pro"}}`)) + ".fixture"
			auth, _ := json.Marshal(map[string]any{"tokens": map[string]any{"id_token": jwt, "access_token": jwt, "refresh_token": "fixture-nonsecret-refresh", "account_id": "fixture-account"}, "last_refresh": time.Now().UTC().Format(time.RFC3339)})
			write("codex/auth.json", string(auth))
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/remote/control/server/enroll") || strings.HasSuffix(r.URL.Path, "/remote/control/server/refresh") {
					t.Logf("GATEWAY ENROLL %s", r.URL.Path)
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(map[string]any{"server_id": "fixture-server", "environment_id": "fixture-environment", "remote_control_token": "fixture-nonsecret-remote-token", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
					return
				}
				if strings.HasSuffix(r.URL.Path, "/remote/control/server") {
					t.Logf("GATEWAY WS %s protocol=%s", r.URL.Path, r.Header.Get("x-codex-protocol-version"))
					conn, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					gatewayCursors <- r.Header.Get("x-codex-subscribe-cursor")
					gatewayConnected <- conn
					return
				}
				if r.Method != "POST" || r.URL.Path != "/responses" {
					t.Logf("IGNORED ancillary fixture route %s %s", r.Method, r.URL.Path)
					http.Error(w, "route", 400)
					return
				}
				raw, err := httputil.ReadBody(r.Body)
				if err != nil {
					t.Error("fixture request read")
					return
				}
				var request map[string]any
				if json.Unmarshal(raw, &request) != nil {
					t.Error("fixture request shape")
					return
				}
				text := string(raw)
				metadata, _ := request["client_metadata"].(map[string]any)
				requestThread, _ := metadata["thread_id"].(string)
				if strings.Contains(text, "CQ_QUALIFICATION_CHILD_HOLD") && requestThread != parentThread.Load().(string) && childHoldIssued.CompareAndSwap(false, true) {
					childObserved <- request
					select {
					case <-childRelease:
					case <-ctx.Done():
						return
					}
				} else if strings.Contains(text, "CQ_QUALIFICATION_SIBLING_HOLD") && siblingHoldIssued.CompareAndSwap(false, true) {
					siblingObserved <- request
					select {
					case <-siblingRelease:
					case <-ctx.Done():
						return
					}
				} else {
					observed <- request
				}
				item := map[string]any{"id": "qualification_reply", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "CQ_QUALIFICATION_REPLY", "annotations": []any{}}}}
				if strings.Contains(text, "CQ_QUALIFICATION_SPAWN_PARENT") && spawnIssued.CompareAndSwap(false, true) {
					args, _ := json.Marshal(map[string]any{"message": "CQ_QUALIFICATION_CHILD_HOLD", "fork_context": false})
					item = map[string]any{"id": "qualification_spawn", "type": "function_call", "namespace": "multi_agent_v1", "name": "spawn_agent", "arguments": string(args), "call_id": "qualification_spawn_call", "status": "completed"}
				}
				w.Header().Set("Content-Type", "text/event-stream")
				for _, ev := range []any{map[string]any{"type": "response.created", "response": map[string]any{"id": "qualification_response", "object": "response", "status": "in_progress", "output": []any{}}}, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item}, map[string]any{"type": "response.completed", "response": map[string]any{"id": "qualification_response", "object": "response", "status": "completed", "output": []any{item}, "usage": map[string]int{"input_tokens": 32, "output_tokens": 1, "total_tokens": 33}}}} {
					b, _ := json.Marshal(ev)
					fmt.Fprintf(w, "data: %s\n\n", b)
				}
			}))
			defer func() {
				childOnce.Do(func() { close(childRelease) })
				siblingOnce.Do(func() { close(siblingRelease) })
				upstream.Close()
			}()
			gateway.origin = upstream.URL
			route := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/backend-api/wham/remote/control/server" {
					gateway.serveHTTP(w, r)
					return
				}
				upstream.Config.Handler.ServeHTTP(w, r)
			}))
			defer route.Close()
			write("codex/config.toml", "chatgpt_base_url = "+fmt.Sprintf("%q", route.URL+"/backend-api")+"\n"+fmt.Sprintf("model = \"glm-5.3\"\nmodel_provider = \"fixture\"\napproval_policy = \"never\"\nsandbox_mode = \"read-only\"\ncli_auth_credentials_store = \"file\"\ncheck_for_update_on_startup = false\n[analytics]\nenabled = false\n[features]\nplugins = true\ncode_mode_host = %t\nenable_request_compression = false\n[model_providers.fixture]\nname = \"Synthetic fixture\"\nbase_url = %q\nwire_api = \"responses\"\nrequires_openai_auth = false\nsupports_websockets = false\nrequest_max_retries = 0\nstream_max_retries = 0\n[mcp_servers.probe]\ncommand = %q\nargs = [\"-test.run=^TestHarnessInstalledMCPHelper$\"]\nstartup_timeout_sec = 5\n[mcp_servers.probe.env]\nCQ_HARNESS_INSTALLED_MCP_HELPER = \"1\"\n", codeMode, upstream.URL, os.Args[0]))
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			address := listener.Addr().String()
			listener.Close()
			profile := `(version 1) (allow default) (deny network*) (deny file-write*) (allow network-outbound (remote ip "localhost:*")) (allow network-bind (local ip "localhost:*")) (allow network-inbound (local ip "localhost:*")) (allow file-write* (literal "/dev/null")) (allow file-write* (subpath ` + fmt.Sprintf("%q", root) + `))`

			cmd := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", "-p", profile, os.Getenv("CQ_DOTS_HARNESS_INSTALLED_CODEX"), "app-server", "--listen", "ws://"+address, "--strict-config")
			cmd.Dir = filepath.Join(root, "work")
			cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin", "LANG=C", "LC_ALL=C", "TERM=dumb", "CI=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "NO_PROXY=127.0.0.1,localhost"}
			for k, v := range map[string]string{"HOME": "home", "CODEX_HOME": "codex", "TMPDIR": "tmp", "XDG_CACHE_HOME": "cache", "XDG_CONFIG_HOME": "config", "XDG_DATA_HOME": "data"} {
				cmd.Env = append(cmd.Env, k+"="+filepath.Join(root, v))
			}
			stderr, _ := os.Create(filepath.Join(root, "native.stderr"))
			defer stderr.Close()
			cmd.Stdout, cmd.Stderr = stderr, stderr
			if err := cmd.Start(); err != nil {
				t.Fatal("fixture native launch")
			}
			defer func() { cmd.Process.Kill(); cmd.Wait() }()
			var direct *websocket.Conn
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
				direct, _, err = websocket.DefaultDialer.Dial("ws://"+address, nil)
				if err == nil {
					break
				}
			}
			if err != nil {
				t.Fatal("fixture native connect")
			}
			defer direct.Close()
			a := newInstalledGatewayDirect(t, h, direct)
			a.call("initialize", map[string]any{"clientInfo": map[string]any{"name": "cq-qualification", "version": "1"}, "capabilities": map[string]any{"experimentalApi": true}})
			a.send(map[string]any{"method": "initialized"})
			a.call("remoteControl/enable", map[string]any{"ephemeral": true})
			var remote *websocket.Conn
			select {
			case remote = <-gatewayConnected:
			case <-time.After(12 * time.Second):
				t.Fatal("native gateway not connected")
			}
			defer remote.Close()
			if cursor := <-gatewayCursors; cursor != "" {
				t.Fatal("initial native gateway unexpectedly had a subscribe cursor")
			}
			frames, err := os.Create(filepath.Join(root, "gateway-frames.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			defer frames.Close()
			b := newInstalledGatewayBackend(t, remote, json.NewEncoder(frames))
			b.call("initialize", map[string]any{"clientInfo": map[string]any{"name": "fixture-backend-notifications", "version": "1"}, "capabilities": map[string]any{"experimentalApi": true}})
			b.send(map[string]any{"method": "initialized"})
			features := a.call("experimentalFeature/list", map[string]any{})
			var featureRows []struct {
				Name    string
				Enabled bool
			}
			json.Unmarshal(features["data"], &featureRows)
			featureFound := false
			for _, f := range featureRows {
				if f.Name == "code_mode_host" {
					featureFound = true
					if f.Enabled != codeMode {
						t.Fatal("native code mode mismatch")
					}
				}
			}
			if !featureFound {
				t.Fatal("native code mode feature unavailable")
			}
			start := func() string {
				v := a.call("thread/start", map[string]any{"cwd": cmd.Dir, "model": "glm-5.3", "modelProvider": "fixture"})
				id := cloudThreadID(cloudHarnessRaw(v))
				if id == "" {
					t.Fatal("native start identity absent")
				}
				return id
			}
			target, sibling := start(), start()
			longHistory := strings.Repeat("h", 180*1024)
			parentThread.Store(target)
			siblingPolicy.Store(sibling)
			var siblingTools string
			checkRequest := func(request map[string]any, id, label string, wantMCP, wantNewSkill bool) {
				raw, _ := json.Marshal(request)
				tools, _ := json.Marshal(request["tools"])
				var schemas []map[string]any
				json.Unmarshal(tools, &schemas)
				hasMCP := false
				for _, schema := range schemas {
					name, _ := schema["name"].(string)
					if strings.HasPrefix(name, "mcp__probe") || name == "mcp" {
						hasMCP = true
					}
				}
				history := strings.Contains(string(raw), "CQ_QUALIFICATION_HISTORY")
				reply := strings.Contains(string(raw), "CQ_QUALIFICATION_REPLY")
				newSkill := strings.Contains(string(raw), "CQ_QUALIFICATION_NEW_SKILL_MARKER")
				if hasMCP != wantMCP || newSkill != wantNewSkill || !history || (id == target && label != "first" && !strings.Contains(string(raw), longHistory)) {
					t.Fatalf("native turn %s mismatch MCP=%t newSkill=%t history=%t", label, hasMCP, newSkill, history)
				}
				if label != "first" && label != "sibling-first" && !reply {
					t.Fatalf("native turn %s lost assistant history", label)
				}
				if id == sibling {
					if siblingTools == "" {
						siblingTools = string(tools)
					} else if siblingTools != string(tools) {
						t.Fatalf("sibling tool schemas changed at %s", label)
					}
				}
				os.WriteFile(filepath.Join(root, label+"-request.json"), raw, 0600)
				t.Logf("TURN %s id=%s mcp=%t history=%t reply=%t new-skill=%t", label, id, hasMCP, history, reply, newSkill)
			}
			turn := func(id, label string, wantMCP, wantNewSkill bool) {
				text := "CQ_QUALIFICATION_STEP_" + label
				if label == "first" {
					text = "CQ_QUALIFICATION_HISTORY CQ_QUALIFICATION_SPAWN_PARENT"
				}
				if label == "long-history" {
					text = "CQ_LONG_HISTORY_BEGIN\n" + longHistory + "\nCQ_LONG_HISTORY_END"
				}
				if label == "sibling-first" {
					text = "CQ_QUALIFICATION_HISTORY"
				}
				a.call("turn/start", map[string]any{"threadId": id, "input": []any{map[string]any{"type": "text", "text": text}}})
				a.turnCompleted(id)
				var latest map[string]any
				select {
				case latest = <-observed:
				case <-time.After(time.Second):
					t.Fatal("no synthetic model request")
				}
				for {
					select {
					case latest = <-observed:
					default:
						checkRequest(latest, id, label, wantMCP, wantNewSkill)
						return
					}
				}
			}
			turn(target, "first", true, false)
			// The second durable turn makes history large while the native
			// thread preview remains short for metadata-only private resumes.
			turn(target, "long-history", true, false)
			turn(sibling, "sibling-first", true, false)
			var childRequest map[string]any
			select {
			case childRequest = <-childObserved:
			case <-time.After(8 * time.Second):
				t.Fatal("synthetic child did not reach held provider request")
			}
			metadata, _ := childRequest["client_metadata"].(map[string]any)
			childID, _ := metadata["thread_id"].(string)
			if childID == "" || childID == target {
				t.Fatal("child identity missing")
			}
			childState := func(label string) {
				loaded := a.call("thread/loaded/list", map[string]any{})
				var ids []string
				json.Unmarshal(loaded["data"], &ids)
				found := false
				for _, id := range ids {
					found = found || id == childID
				}
				if !found {
					t.Fatalf("live child unloaded during %s", label)
				}
				v := a.call("thread/read", map[string]any{"threadId": childID, "includeTurns": false})
				var child struct {
					Thread struct{ ID, ParentThreadID, SessionID string }
				}
				json.Unmarshal(cloudHarnessRaw(v), &child)
				if child.Thread.ID != childID || child.Thread.ParentThreadID != target || child.Thread.SessionID != target {
					t.Fatalf("child tree identity changed at %s", label)
				}
				t.Logf("LIVE CHILD %s id=%s parent=%s preserved", label, childID, target)
			}
			childState("before-reload")
			resumed := b.call("thread/resume", map[string]any{"threadId": target, "excludeTurns": false})
			if cloudThreadID(cloudHarnessRaw(resumed)) != target || !strings.Contains(string(cloudHarnessRaw(resumed)), longHistory) || b.chunkedReplies.Load() != 1 || b.chunks.Load() < 2 || b.chunkedBytes.Load() <= 150*1024 {
				t.Fatal("native full-history resume did not produce a verified large chunked response")
			}
			t.Logf("NATIVE LONG HISTORY chunks=%d message-bytes=%d same-id=%s", b.chunks.Load(), b.chunkedBytes.Load(), target)
			b.call("thread/resume", map[string]any{"threadId": sibling, "excludeTurns": true})
			source := cloudHarnessGatewaySource(b.client, b.stream)
			h.mu.Lock()
			if !h.threads[target].subscriptions["stdio"] || h.threads[target].subscriptions[source] || !h.threads[sibling].subscriptions["stdio"] || !h.threads[sibling].subscriptions[source] {
				h.mu.Unlock()
				t.Fatal("chunked target response unexpectedly established observed gateway ownership")
			}
			h.mu.Unlock()
			if string(gateway.ResumeParams(source, target)["excludeTurns"]) != "false" {
				t.Fatal("gateway lost full-history resume candidate metadata")
			}
			a.call("thread/goal/set", map[string]any{"threadId": target, "objective": "CQ_SYNTHETIC_PAUSED_GOAL", "status": "paused", "tokenBudget": 10000, "origin": "user"})
			beforeGoal := a.call("thread/goal/get", map[string]any{"threadId": target})["goal"]
			b.call("turn/start", map[string]any{"threadId": sibling, "input": []any{map[string]any{"type": "text", "text": "CQ_QUALIFICATION_SIBLING_HOLD"}}})
			select {
			case request := <-siblingObserved:
				checkRequest(request, sibling, "sibling-held", true, false)
			case <-time.After(8 * time.Second):
				t.Fatal("sibling did not reach held provider request")
			}
			write("codex/skills/new-qualification/SKILL.md", "---\nname: new-qualification\ndescription: CQ_QUALIFICATION_NEW_SKILL_MARKER\n---\nSynthetic new skill.\n")
			status := func(id, label string, want bool) {
				runtime, err := h.readMCPRuntime(id)
				if err != nil {
					t.Fatalf("MCP readback: %v", err)
				}
				if (runtime["probe"].Exposed > 0) != want {
					t.Fatalf("MCP %s runtime mismatch: %#v", label, runtime)
				}
				plugins, err := h.readPluginRuntime(id)
				if err != nil || plugins != want {
					t.Fatalf("plugin %s runtime mismatch: enabled=%t err=%v", label, plugins, err)
				}
			}
			reconcile := func(enabled bool, label string) {
				policyEnabled.Store(enabled)
				before := b.inputSeq
				h.op.Lock()
				err := h.reconcile(target, "", "")
				h.op.Unlock()
				if err != nil {
					t.Fatalf("actual reconcile %s: %v", label, err)
				}
				if b.inputSeq != before {
					t.Fatal("private reconciliation consumed original backend input sequence")
				}
				h.mu.Lock()
				thread := h.threads[target]
				if thread.minimal != enabled || thread.receiptState != "applied" || len(thread.runtimeIssues) != 0 || !thread.subscriptions["stdio"] {
					failure := fmt.Sprintf("harness %s did not apply/restore native subscriptions: minimal=%t receipt=%s issues=%v subscriptions=%v", label, thread.minimal, thread.receiptState, thread.runtimeIssues, thread.subscriptions)
					h.mu.Unlock()
					t.Fatal(failure)
				}
				h.mu.Unlock()
				afterGoal := a.call("thread/goal/get", map[string]any{"threadId": target})["goal"]
				if string(beforeGoal) != string(afterGoal) {
					t.Fatal("paused goal changed")
				}
				status(target, label, !enabled)
				status(sibling, "sibling-"+label, true)
				childState(label)
				t.Logf("ACTUAL HARNESS %s applied target=%s original-input-seq=%d", label, target, b.inputSeq)
			}
			reconcile(true, "minimal")
			turn(target, "minimal", false, false)
			b.turnCompleted(target)
			reconcile(false, "restored-full")
			turn(target, "restored-full", true, true)
			b.turnCompleted(target)
			siblingOnce.Do(func() { close(siblingRelease) })
			b.turnCompleted(sibling)
			a.turnCompleted(sibling)
			childOnce.Do(func() { close(childRelease) })
			a.turnCompleted(childID)
			childState("after-held-child-completed")
			turn(sibling, "sibling-after-cycles", true, true)
			// A real private native response crosses the actual handler. Hold
			// its Pong ACK, reconnect, and require the same surrogate at the
			// same native sequence before acknowledging it cumulatively.
			b.holdPong.Store(true)
			if _, err := h.callOn(source, "thread/resume", map[string]any{"threadId": target, "excludeTurns": true}); err != nil {
				t.Fatal(err)
			}
			var firstPong json.RawMessage
			select {
			case firstPong = <-b.held:
			case <-time.After(5 * time.Second):
				t.Fatal("gateway did not produce private Pong")
			}
			wantCursor := fmt.Sprintf("fixture-ack-%d", b.lastAck.Load())
			b.c.Close()
			var next *websocket.Conn
			select {
			case next = <-gatewayConnected:
			case <-time.After(12 * time.Second):
				t.Fatal("native gateway did not reconnect")
			}
			defer next.Close()
			if cursor := <-gatewayCursors; cursor != wantCursor {
				t.Fatalf("native reconnect cursor=%s want=%s", cursor, wantCursor)
			}
			b.c = next
			b.replay = firstPong
			b.read()
			b.call("thread/loaded/list", map[string]any{})
			t.Logf("ACTUAL GATEWAY REPLAY PASSED private-pongs=%d server-seq=%d original-input-seq=%d target=%s sibling=%s child=%s", b.pongs.Load(), b.lastServer.Load(), b.inputSeq, target, sibling, childID)
		})
	}
}
