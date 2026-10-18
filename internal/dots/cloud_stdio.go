package dots

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

const cloudAppMarker = "plugins.codex-app-tools@openai-bundled.mcp_servers.codex_app.enabled="

var cloudReadMethods = map[string]bool{
	"thread/resume": true, "thread/read": true, "thread/list": true,
	"thread/loaded/list": true, "thread/turns/list": true, "thread/items/list": true,
	"model/list": true, "collaborationMode/list": true, "account/rateLimits/read": true,
	"thread/realtime/listVoices": true,
}

type cloudMessage map[string]json.RawMessage

func (m cloudMessage) method() string {
	var method string
	_ = json.Unmarshal(m["method"], &method)
	return method
}

func cloudThreadID(raw json.RawMessage) string {
	var value cloudMessage
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	var identifier string
	if json.Unmarshal(value["threadId"], &identifier) == nil && bytes.HasPrefix(bytes.TrimSpace(value["threadId"]), []byte{'"'}) {
		return identifier
	}
	var thread cloudMessage
	if json.Unmarshal(value["thread"], &thread) != nil {
		return ""
	}
	_ = json.Unmarshal(thread["id"], &identifier)
	return identifier
}

// CloudStdioExitError preserves a stock CLI's exit status through the helper.
type CloudStdioExitError struct{ Code int }

func (e *CloudStdioExitError) Error() string {
	return fmt.Sprintf("stock Codex CLI exited with status %d", e.Code)
}

type cloudLocal struct {
	input  io.WriteCloser
	output io.ReadCloser
	wait   func() error
	stop   func()
}

type cloudSocket interface {
	ReadMessage() (int, []byte, error)
	WriteMessage(int, []byte) error
	SetReadDeadline(time.Time) error
	Close() error
}

type cloudStdioDependencies struct {
	environment    []string
	launch         func(context.Context, string, []string, []string, io.Writer) (*cloudLocal, error)
	stock          func(context.Context, string, []string, []string, io.Reader, io.Writer, io.Writer) error
	dial           func(context.Context, Account) (cloudSocket, int, error)
	resolveOwner   func(string) (Account, error)
	http           cloudHTTPDoer
	readRoots      func(string, string) ([]byte, error)
	event          func(map[string]any)
	statusInterval time.Duration
	now            func() time.Time
}

func startCloudLocal(ctx context.Context, path string, arguments, environment []string, stderr io.Writer) (*cloudLocal, error) {
	command := exec.CommandContext(ctx, path, arguments...)
	command.Env, command.Stderr = environment, stderr
	input, err := command.StdinPipe()
	if err != nil {
		return nil, errors.New("stock Codex CLI input unavailable")
	}
	output, err := command.StdoutPipe()
	if err != nil {
		_ = input.Close()
		return nil, errors.New("stock Codex CLI output unavailable")
	}
	if err := command.Start(); err != nil {
		_ = input.Close()
		_ = output.Close()
		return nil, errors.New("stock Codex CLI launch failed")
	}
	return &cloudLocal{input: input, output: output, wait: command.Wait, stop: func() {
		_ = input.Close()
		_ = command.Process.Kill()
		_ = output.Close()
	}}, nil
}

func cloudStock(ctx context.Context, path string, arguments, environment []string, stdin io.Reader, stdout, stderr io.Writer) error {
	command := exec.CommandContext(ctx, path, arguments...)
	command.Env, command.Stdin, command.Stdout, command.Stderr = environment, stdin, stdout, stderr
	if err := command.Run(); err != nil {
		var exited *exec.ExitError
		if errors.As(err, &exited) {
			return &CloudStdioExitError{Code: cloudExitCode(exited)}
		}
		return errors.New("stock Codex CLI launch failed")
	}
	return nil
}

func cloudExitCode(exited *exec.ExitError) int {
	if status, ok := exited.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	if code := exited.ExitCode(); code >= 0 {
		return code
	}
	return 1
}

func nativeCloudArguments(arguments, environment []string) ([]string, error) {
	base, exists := "", false
	for _, value := range environment {
		if strings.HasPrefix(value, "OPENAI_BASE_URL=") {
			base, exists = strings.TrimPrefix(value, "OPENAI_BASE_URL="), true
		}
	}
	if !exists {
		return arguments, nil
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return nil, errors.New("native CQ model proxy must be a loopback HTTP endpoint")
	}
	port, portErr := strconv.Atoi(parsed.Port())
	if parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.User != nil || parsed.Opaque != "" ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		portErr != nil || port < 1 || port > 65535 {
		return nil, errors.New("native CQ model proxy must be a loopback HTTP endpoint")
	}
	encoded, _ := json.Marshal(base)
	return append(append([]string(nil), arguments...), "-c", "openai_base_url="+string(encoded)), nil
}

// RunCloudStdio keeps ordinary and primary RPCs on the stock CLI. Only verified
// Dot-root reads cross into their pinned owner's cloud session. Credentials are
// never passed to the stock child or written to diagnostics.
func RunCloudStdio(ctx context.Context, cfg *Settings, accounts []Account, arguments []string, stdin io.Reader, stdout, stderr io.Writer, evaluator NativeEvaluator) error {
	_ = evaluator // The bridge does not evaluate or alter the renderer.
	if cfg == nil {
		return errors.New("Dots settings unavailable")
	}
	return runCloudStdio(ctx, cfg, accounts, arguments, stdin, stdout, stderr, cloudStdioDefaults(cfg))
}

type cloudPending struct {
	id       json.RawMessage
	method   string
	resource string
}

type cloudSession struct {
	alias   string
	socket  cloudSocket
	write   sync.Mutex
	pending map[string]cloudPending // protected by cloudBridge.sessionsMu
	closed  bool
	cancel  context.CancelFunc
	started time.Time
}

type cloudWork struct {
	request    cloudMessage
	raw        []byte
	resource   string
	initialize cloudMessage
}

type cloudBridge struct {
	ctx           context.Context
	cancel        context.CancelFunc
	cfg           *Settings
	deps          cloudStdioDependencies
	owners        map[string]Account
	credentialsMu sync.Mutex
	credentials   map[string]Account
	output        io.Writer
	writeMu       sync.Mutex
	outputErr     chan error
	local         *cloudLocal
	localWrite    sync.Mutex
	rootsMu       sync.Mutex
	roots         map[string]string
	sessionsMu    sync.Mutex
	sessions      map[string]*cloudSession
	queues        map[string]chan cloudWork // owned by the stdin routing loop
	workers       sync.WaitGroup
	statusMu      sync.Mutex
	statusWatches map[string]bool
	statusSeen    map[string]json.RawMessage
}

func runCloudStdio(ctx context.Context, cfg *Settings, accounts []Account, arguments []string, stdin io.Reader, stdout, stderr io.Writer, deps cloudStdioDependencies) error {
	path := filepath.Join(cfg.AppPath, "Contents", "Resources", "codex-cli", "CodexCLI.app", "Contents", "MacOS", "codex")
	environment := setEnvironment(deps.environment, "CODEX_CLI_PATH", path)
	appServer, appSession := false, false
	for _, argument := range arguments {
		appServer = appServer || argument == "app-server"
		appSession = appSession || argument == cloudAppMarker+"true" || argument == cloudAppMarker+"false"
	}
	var err error
	if appServer {
		arguments, err = nativeCloudArguments(arguments, environment)
		if err != nil {
			return err
		}
	}
	if !appServer || !appSession {
		return deps.stock(ctx, path, arguments, environment, stdin, stdout, stderr)
	}
	ctx, cancel := context.WithCancel(ctx)
	b := &cloudBridge{ctx: ctx, cancel: cancel, cfg: cfg, deps: deps, output: stdout,
		outputErr: make(chan error, 1), owners: make(map[string]Account), roots: make(map[string]string),
		sessions: make(map[string]*cloudSession), queues: make(map[string]chan cloudWork),
		statusWatches: make(map[string]bool), statusSeen: make(map[string]json.RawMessage)}
	defer cancel()
	b.event(map[string]any{"app_stdio_adapter_started": true, "read_only": false, "app_session": true, "integration_mode": "native"})
	b.loadOwners(accounts)
	for _, dot := range cfg.Dots {
		if dot.RootThreadID != "" {
			b.roots[dot.RootThreadID] = dot.OwnerAlias
		}
	}
	b.local, err = deps.launch(ctx, path, arguments, environment, stderr)
	if err != nil {
		return err
	}
	localExit := make(chan struct{})
	var localErr error
	var localWait sync.Once
	startLocalWait := func() {
		localWait.Do(func() {
			go func() {
				defer close(localExit)
				defer func() {
					if recover() != nil {
						localErr = errors.New("stock Codex CLI wait failed")
					}
				}()
				localErr = b.local.wait()
			}()
		})
	}
	defer func() {
		cancel()
		b.sessionsMu.Lock()
		closing := make([]*cloudSession, 0, len(b.sessions))
		for _, session := range b.sessions {
			closing = append(closing, session)
		}
		b.sessionsMu.Unlock()
		for _, session := range closing {
			b.closeSession(session)
		}
		b.local.stop()
		startLocalWait()
		select {
		case <-localExit:
		case <-time.After(5 * time.Second):
		}
		b.workers.Wait()
	}()
	localClosed := make(chan struct{})
	go b.guard("local", func() {
		defer close(localClosed)
		reader := bufio.NewReader(b.local.output)
		for {
			raw, readErr := reader.ReadBytes('\n')
			if len(raw) != 0 {
				b.emitRaw(raw)
			}
			if readErr != nil {
				return
			}
		}
	})
	type input struct {
		raw []byte
		err error
	}
	incoming := make(chan input, 1)
	go b.guard("stdin", func() {
		reader := bufio.NewReader(stdin)
		for {
			raw, readErr := reader.ReadBytes('\n')
			if len(raw) != 0 {
				select {
				case incoming <- input{raw: raw}:
				case <-ctx.Done():
					return
				}
			}
			if readErr != nil {
				select {
				case incoming <- input{err: readErr}:
				case <-ctx.Done():
				}
				return
			}
		}
	})
	var initialization cloudMessage
	initialized := false
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-localClosed:
			startLocalWait()
			select {
			case <-localExit:
				if localErr == nil {
					return nil
				}
				var exited *exec.ExitError
				if errors.As(localErr, &exited) {
					return &CloudStdioExitError{Code: cloudExitCode(exited)}
				}
				return localErr
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
				return &CloudStdioExitError{Code: 1}
			}
		case err := <-b.outputErr:
			return err
		case next := <-incoming:
			if next.err != nil {
				if errors.Is(next.err, io.EOF) {
					return nil
				}
				return errors.New("native request input failed")
			}
			var request cloudMessage
			if json.Unmarshal(next.raw, &request) != nil || request == nil {
				return errors.New("native request JSON invalid")
			}
			method := request.method()
			if method == "initialize" {
				initialization = request
			}
			if method == "initialized" {
				initialized = true
			}
			resource := cloudThreadID(request["params"])
			alias := b.threadOwner(resource)
			if cloudReadMethods[method] && alias != "" {
				if !initialized || initialization == nil {
					b.fail(request, "Owner cloud routing verification unavailable")
					continue
				}
				if _, ok := b.owners[alias]; !ok {
					b.fail(request, "Owner cloud routing verification unavailable")
					continue
				}
				queue := b.queues[alias]
				if queue == nil {
					queue = make(chan cloudWork, 32)
					b.queues[alias] = queue
					b.workers.Add(1)
					go b.ownerWorker(alias, queue)
				}
				select {
				case queue <- cloudWork{request: request, raw: next.raw, resource: resource, initialize: initialization}:
				default:
					b.fail(request, "Owner cloud request queue is full")
				}
			} else if method != "" && alias != "" && alias != cfg.SystemAlias {
				b.event(map[string]any{"blocked_method": method, "owner_alias": alias})
				b.fail(request, "Secondary Dot operation is not supported")
			} else {
				b.localWrite.Lock()
				_, err := b.local.input.Write(next.raw)
				b.localWrite.Unlock()
				if err != nil {
					return errors.New("stock Codex CLI input closed")
				}
			}
		}
	}
}

func (b *cloudBridge) loadOwners(accounts []Account) {
	expected := map[string]bool{b.cfg.SystemAlias: true}
	for _, dot := range b.cfg.Dots {
		expected[dot.OwnerAlias] = true
	}
	valid := true
	for _, account := range accounts {
		if !expected[account.Alias] {
			continue
		}
		if _, duplicate := b.owners[account.Alias]; duplicate || account.AccountID == "" || account.Token == "" {
			valid = false
		}
		b.owners[account.Alias] = account
	}
	for alias := range expected {
		if _, ok := b.owners[alias]; !ok {
			valid = false
		}
	}
	if system, ok := b.owners[b.cfg.SystemAlias]; !ok || (b.cfg.SystemAccountID != "" && system.AccountID != b.cfg.SystemAccountID) {
		valid = false
	}
	for _, dot := range b.cfg.Dots {
		if owner, ok := b.owners[dot.OwnerAlias]; !ok || (dot.OwnerAccountID != "" && owner.AccountID != dot.OwnerAccountID) {
			valid = false
		}
	}
	if !valid {
		b.owners = map[string]Account{}
		b.event(map[string]any{"owner_inventory_failure_type": "validation"})
	}
}

func (b *cloudBridge) guard(source string, fn func()) {
	defer func() {
		if recover() != nil {
			b.event(map[string]any{"owner_alias": source, "receive_failure_type": "panic"})
		}
	}()
	fn()
}

func (b *cloudBridge) event(value map[string]any) {
	if b.deps.event != nil {
		b.deps.event(value)
	}
}

func (b *cloudBridge) emitRaw(raw []byte) {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		raw = append(append([]byte(nil), raw...), '\n')
	}
	if _, err := b.output.Write(raw); err != nil {
		select {
		case b.outputErr <- errors.New("native response output failed"):
		default:
		}
	}
}

func (b *cloudBridge) emit(value any) {
	raw, err := json.Marshal(value)
	if err == nil {
		b.emitRaw(raw)
	}
}

func (b *cloudBridge) fail(request cloudMessage, message string) {
	if id, ok := request["id"]; ok {
		b.emit(map[string]any{"id": id, "error": map[string]any{"code": -32000, "message": message}})
	}
}

func (b *cloudBridge) threadOwner(resource string) string {
	if resource == "" {
		return ""
	}
	b.rootsMu.Lock()
	defer b.rootsMu.Unlock()
	if alias := b.roots[resource]; alias != "" {
		return alias
	}
	data, err := b.deps.readRoots(filepath.Join(b.cfg.RuntimeRoot, "native-roots.json"), b.cfg.RuntimeRoot)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			b.event(map[string]any{"native_root_snapshot_failure_type": "validation"})
		}
		return ""
	}
	var snapshot struct {
		Version int `json:"version"`
		Roots   []struct {
			OwnerAlias   string `json:"owner_alias"`
			ProfileID    string `json:"profile_id"`
			RootThreadID string `json:"root_thread_id"`
		} `json:"roots"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&snapshot) != nil || snapshot.Version != 1 || len(snapshot.Roots) != len(b.cfg.ProfileIDs()) {
		b.event(map[string]any{"native_root_snapshot_failure_type": "validation"})
		return ""
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		b.event(map[string]any{"native_root_snapshot_failure_type": "validation"})
		return ""
	}
	expected, aliases, roots := b.cfg.ProfileIDs(), map[string]bool{}, map[string]string{}
	for _, row := range snapshot.Roots {
		if profile, ok := expected[row.OwnerAlias]; !ok || aliases[row.OwnerAlias] || row.ProfileID != profile || row.RootThreadID == "" || len(row.RootThreadID) > 128 || roots[row.RootThreadID] != "" {
			b.event(map[string]any{"native_root_snapshot_failure_type": "validation"})
			return ""
		}
		if previous := b.roots[row.RootThreadID]; previous != "" && previous != row.OwnerAlias {
			b.event(map[string]any{"native_root_snapshot_failure_type": "validation"})
			return ""
		}
		aliases[row.OwnerAlias], roots[row.RootThreadID] = true, row.OwnerAlias
	}
	for root, alias := range roots {
		b.roots[root] = alias
	}
	return b.roots[resource]
}

func (b *cloudBridge) ownerWorker(alias string, queue <-chan cloudWork) {
	defer b.workers.Done()
	b.guard(alias, func() {
		for {
			select {
			case <-b.ctx.Done():
				return
			case work := <-queue:
				if b.ctx.Err() != nil {
					return
				}
				err := func() (err error) {
					defer func() {
						if recover() != nil {
							err = errors.New("owner cloud routing panicked")
						}
					}()
					session, err := b.openSession(alias, work.initialize)
					if err != nil {
						return err
					}
					b.event(map[string]any{"routed_method": work.request.method(), "owner_alias": alias})
					if method := work.request.method(); method == "thread/read" || method == "thread/turns/list" || method == "thread/items/list" {
						b.httpRead(alias, work.request, work.resource)
						return nil
					}
					return b.send(session, work)
				}()
				if err != nil && b.ctx.Err() == nil {
					b.event(map[string]any{"owner_routing_failure_type": "unavailable"})
					b.fail(work.request, "Owner cloud routing verification unavailable")
				}
			}
		}
	})
}

func (b *cloudBridge) openSession(alias string, initialize cloudMessage) (*cloudSession, error) {
	b.sessionsMu.Lock()
	session := b.sessions[alias]
	b.sessionsMu.Unlock()
	if session != nil {
		return session, nil
	}
	owner, err := b.ownerCredential(alias)
	if err != nil {
		return nil, err
	}
	socket, status, err := b.deps.dial(b.ctx, owner)
	if err != nil {
		return nil, errors.New("owner cloud session unavailable")
	}
	accepted := false
	stopClosing := context.AfterFunc(b.ctx, func() { b.guard(alias, func() { _ = socket.Close() }) })
	defer func() {
		if !accepted {
			stopClosing()
			_ = socket.Close()
		}
	}()
	internal := make(cloudMessage, len(initialize))
	for key, value := range initialize {
		internal[key] = value
	}
	internal["id"], _ = json.Marshal("cq-internal-initialize-" + alias)
	raw, _ := json.Marshal(internal)
	if socket.SetReadDeadline(time.Now().Add(30*time.Second)) != nil || socket.WriteMessage(websocket.TextMessage, raw) != nil {
		return nil, errors.New("owner cloud initialization unavailable")
	}
	for {
		_, raw, readErr := socket.ReadMessage()
		if readErr != nil {
			return nil, errors.New("owner cloud initialization unavailable")
		}
		var response cloudMessage
		if json.Unmarshal(raw, &response) != nil {
			return nil, errors.New("owner cloud initialization invalid")
		}
		if !bytes.Equal(response["id"], internal["id"]) {
			continue
		}
		if _, rejected := response["error"]; rejected {
			return nil, errors.New("owner cloud initialization rejected")
		}
		break
	}
	if socket.SetReadDeadline(time.Time{}) != nil || socket.WriteMessage(websocket.TextMessage, []byte(`{"method":"initialized"}`)) != nil {
		return nil, errors.New("owner cloud initialization unavailable")
	}
	socketContext, cancel := context.WithCancel(b.ctx)
	session = &cloudSession{alias: alias, socket: socket, pending: make(map[string]cloudPending), started: b.deps.now(), cancel: func() { cancel(); stopClosing() }}
	b.sessionsMu.Lock()
	if b.ctx.Err() != nil {
		b.sessionsMu.Unlock()
		return nil, b.ctx.Err()
	}
	b.sessions[alias] = session
	b.sessionsMu.Unlock()
	accepted = true
	b.event(map[string]any{"owner_alias": alias, "upgrade_status": status, "initialize_success": true})
	if ws, ok := socket.(*websocket.Conn); ok {
		socketKeepalive(socketContext, ws, func() { b.closeSession(session) })
	}
	b.workers.Add(1)
	go func() {
		defer b.workers.Done()
		b.guard(alias, func() { defer b.closeSession(session); b.receive(session) })
	}()
	return session, nil
}

func (b *cloudBridge) send(session *cloudSession, work cloudWork) error {
	id, hasID := work.request["id"]
	key := string(id)
	b.sessionsMu.Lock()
	if session.closed {
		b.sessionsMu.Unlock()
		return errors.New("owner cloud session closed")
	}
	if hasID {
		session.pending[key] = cloudPending{id: id, method: work.request.method(), resource: work.resource}
	}
	b.sessionsMu.Unlock()
	session.write.Lock()
	err := func() (err error) {
		defer func() {
			if recover() != nil {
				err = errors.New("owner cloud write panicked")
			}
		}()
		return session.socket.WriteMessage(websocket.TextMessage, bytes.TrimSuffix(work.raw, []byte{'\n'}))
	}()
	session.write.Unlock()
	if err == nil {
		return nil
	}
	if hasID {
		b.sessionsMu.Lock()
		_, expected := session.pending[key]
		delete(session.pending, key)
		b.sessionsMu.Unlock()
		if !expected {
			return nil
		} // Reader already answered or failed this request.
	}
	b.closeSession(session)
	return errors.New("owner cloud send failed")
}

func (b *cloudBridge) closeSession(session *cloudSession) {
	b.sessionsMu.Lock()
	if session.closed {
		b.sessionsMu.Unlock()
		return
	}
	session.closed = true
	if session.cancel != nil {
		session.cancel()
	}
	if b.sessions[session.alias] == session {
		delete(b.sessions, session.alias)
	}
	lost := session.pending
	session.pending = make(map[string]cloudPending)
	b.sessionsMu.Unlock()
	func() { defer func() { _ = recover() }(); _ = session.socket.Close() }()
	if b.ctx.Err() == nil {
		for _, pending := range lost {
			b.fail(cloudMessage{"id": pending.id}, "Owner cloud session closed before responding")
		}
	}
}

func (b *cloudBridge) receive(session *cloudSession) {
	for {
		_, raw, err := session.socket.ReadMessage()
		if err != nil {
			if b.ctx.Err() == nil {
				code := 0
				var closed *websocket.CloseError
				if errors.As(err, &closed) {
					code = closed.Code
				}
				b.event(map[string]any{"owner_alias": session.alias, "receive_failure_type": "closed",
					"close_code": code, "reason_type": celsiusErrorType(err), "lifetime_ms": max(int64(0), b.deps.now().Sub(session.started).Milliseconds())})
			}
			return
		}
		var value cloudMessage
		if json.Unmarshal(raw, &value) != nil || value == nil {
			b.event(map[string]any{"owner_alias": session.alias, "receive_failure_type": "invalid_json"})
			return
		}
		if id, exists := value["id"]; exists {
			b.sessionsMu.Lock()
			pending, expected := session.pending[string(id)]
			delete(session.pending, string(id))
			b.sessionsMu.Unlock()
			if !expected {
				continue
			}
			returned := cloudThreadID(value["result"])
			if pending.resource != "" && returned != "" && returned != pending.resource {
				b.fail(cloudMessage{"id": pending.id}, "Cloud response resource mismatch")
				b.event(map[string]any{"owner_alias": session.alias, "method": pending.method, "resource_mismatch": true})
				continue
			}
			b.emitRaw(raw)
			if _, failed := value["error"]; !failed && pending.method == "thread/resume" && pending.resource != "" && session.alias != b.cfg.SystemAlias {
				b.watchStatus(session.alias, pending.resource)
			}
			continue
		}
		if session.alias == b.cfg.SystemAlias {
			b.emitRaw(raw)
			continue
		}
		resource := cloudThreadID(value["params"])
		b.rootsMu.Lock()
		owner := b.roots[resource]
		b.rootsMu.Unlock()
		if resource != "" && owner == session.alias {
			if value.method() == "thread/status/changed" {
				var params cloudMessage
				_ = json.Unmarshal(value["params"], &params)
				b.statusMu.Lock()
				b.statusSeen[resource] = params["status"]
				b.statusMu.Unlock()
			}
			b.emitRaw(raw)
		}
	}
}
