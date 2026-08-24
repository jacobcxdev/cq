package proxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/modelregistry"
)

func TestCodexInstalledSupervisorSupportsRemoteCompaction(t *testing.T) {
	if os.Getenv("CQ_RUN_CODEX_TASK_AFFINITY_ACCEPTANCE") != "1" {
		t.Skip("installed Codex supervisor acceptance requires explicit opt-in")
	}
	clientPath, err := resolveCodexAcceptanceClientExecutable()
	if err != nil {
		t.Fatalf("resolve installed Codex client: %v", err)
	}
	clientProof, err := captureCodexInstalledExecutable(clientPath)
	if err != nil {
		t.Fatalf("capture installed Codex client: %v", err)
	}
	core, err := newCodexInstalledHTTPValidationRuntimeCore(context.Background())
	if err != nil {
		t.Fatalf("open authoritative validation runtime: %v", err)
	}
	t.Cleanup(func() {
		if err := core.close(); err != nil {
			t.Errorf("close authoritative validation runtime: %v", err)
		}
	})
	localToken, err := newCodexInstalledHTTPValidationToken()
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		Config: &Config{
			LocalToken:     localToken,
			ClaudeUpstream: "http://" + core.upstream.address,
			CodexUpstream:  "http://" + core.upstream.address,
		},
		CodexNativeHTTP: core.nativeHTTPHandler(),
		Catalog:         modelregistry.NewCatalog(modelregistry.Snapshot{}),
	}
	handler, err := server.RuntimeHandler()
	if err != nil {
		t.Fatalf("construct candidate runtime handler: %v", err)
	}
	listener, _ := newCodexRuntimeSupervisorAcceptanceServerWithCredential(t, handler, NormalCallerCredentialV1{
		Domain: NormalCallerCodex, Bearer: localToken, SubjectID: "validation-codex",
		ValidUntil: time.Now().Add(time.Hour),
	})

	isolation := newCodexTaskAffinityAcceptanceIsolation(t, localToken)
	runner := codexTaskAffinityAcceptanceRunner{}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := runCodexCompactionAcceptanceTurn(ctx, runner, clientProof, listener.URL, isolation, false, "Reply with exactly PONG and no other text.", "PONG"); err != nil {
		t.Fatalf("first Codex compaction turn: %v", err)
	}
	if err := runCodexCompactionAcceptanceTurn(ctx, runner, clientProof, listener.URL, isolation, true, "Reply with exactly PONG and no other text.", "PONG"); err != nil {
		t.Fatalf("second Codex compaction turn: %v", err)
	}
	core.upstream.mu.Lock()
	compact := core.upstream.compact
	core.upstream.mu.Unlock()
	if compact == 0 {
		t.Fatal("installed Codex did not issue remote compaction request")
	}
}

func TestCodexInstalledRescuePassesThroughCurrentClient(t *testing.T) {
	if os.Getenv("CQ_RUN_CODEX_TASK_AFFINITY_ACCEPTANCE") != "1" {
		t.Skip("installed Codex rescue acceptance requires explicit opt-in")
	}
	clientPath, err := resolveCodexAcceptanceClientExecutable()
	if err != nil {
		t.Fatalf("resolve installed Codex client: %v", err)
	}
	clientProof, err := captureCodexInstalledExecutable(clientPath)
	if err != nil {
		t.Fatalf("capture installed Codex client: %v", err)
	}
	localToken, err := newCodexInstalledHTTPValidationToken()
	if err != nil {
		t.Fatal(err)
	}
	upstreamToken, err := newCodexInstalledHTTPValidationToken()
	if err != nil {
		t.Fatal(err)
	}
	workerHandler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "normal worker used during rescue", http.StatusInternalServerError)
	})
	listener, supervisor := newCodexRuntimeSupervisorAcceptanceServer(t, workerHandler, localToken)

	var upstreamMu sync.Mutex
	upstreamModels := 0
	upstreamResponses := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+upstreamToken {
			http.Error(writer, "unexpected authentication", http.StatusUnauthorized)
			return
		}
		upstreamMu.Lock()
		defer upstreamMu.Unlock()
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/backend-api/codex/models":
			upstreamModels++
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"models":[]}`)
		case request.Method == http.MethodPost && request.URL.Path == "/backend-api/codex/responses":
			upstreamResponses++
			writer.Header().Set("Content-Type", "text/event-stream")
			writer.Header()["X-Codex-Secondary-Reset-At"] = []string{""}
			_, _ = io.WriteString(writer, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"rescue-validation-response\"}}\n\n")
			_, _ = io.WriteString(writer, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"rescue-validation-message\",\"content\":[{\"type\":\"output_text\",\"text\":\"PONG\"}]}}\n\n")
			_, _ = io.WriteString(writer, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"rescue-validation-response\",\"end_turn\":true,\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"total_tokens\":0}}}\n\n")
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(upstream.Close)
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := url.Parse("https://chatgpt.com/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	listenerURL, err := url.Parse(listener.URL)
	if err != nil {
		t.Fatal(err)
	}
	fairnessKey := sha256.Sum256([]byte("installed-codex-rescue-acceptance"))
	relay := &RescueRelay{
		Transport: &http.Client{Transport: &codexRescueAcceptanceTransport{target: target, inner: http.DefaultTransport}},
		Origin:    origin, LoopbackHost: listenerURL.Host, ForwardingAcknowledged: true,
		DenyBearer: func(bearer []byte) bool { return string(bearer) == localToken },
		Budget:     NewRescueBudget(time.Now, fairnessKey),
	}
	if err := supervisor.ConfigureRescue(context.Background(), relay, &runtimeEvidenceTestStore{}); err != nil {
		t.Fatal(err)
	}
	enter, err := http.NewRequest(http.MethodPost, listener.URL+RuntimeRescueEnterPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	enter.Header.Set("Authorization", "Bearer "+localToken)
	enterResponse, err := http.DefaultClient.Do(enter)
	if err != nil {
		t.Fatal(err)
	}
	_ = enterResponse.Body.Close()
	if enterResponse.StatusCode != http.StatusOK {
		t.Fatalf("enter rescue = %d mode %q", enterResponse.StatusCode, supervisor.TrafficMode())
	}
	waitForRuntimeMode(t, supervisor, TrafficModeRescue)

	isolation := newCodexTaskAffinityAcceptanceIsolation(t, upstreamToken)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := runCodexTaskAffinityAcceptanceTurn(ctx, codexTaskAffinityAcceptanceRunner{}, clientProof, listener.URL, isolation, false, "PONG"); err != nil {
		t.Fatalf("Codex rescue turn: %v", err)
	}
	upstreamMu.Lock()
	models := upstreamModels
	responses := upstreamResponses
	upstreamMu.Unlock()
	if models == 0 || responses != 1 {
		t.Fatalf("rescue upstream calls = models %d responses %d", models, responses)
	}
}

func TestCodexInstalledRescuePassesThroughLiveUpstream(t *testing.T) {
	if os.Getenv("CQ_RUN_CODEX_LIVE_UPSTREAM_ACCEPTANCE") != "1" {
		t.Skip("live Codex rescue acceptance requires explicit opt-in")
	}
	authPath := os.Getenv("CQ_CODEX_LIVE_AUTH_FILE")
	if !filepath.IsAbs(authPath) {
		t.Fatal("live Codex auth file must be absolute")
	}
	auth, err := os.ReadFile(authPath)
	if err != nil || len(auth) == 0 || len(auth) > 1<<20 || !json.Valid(auth) {
		t.Fatal("live Codex auth file is unavailable or invalid")
	}
	clientPath, err := resolveCodexAcceptanceClientExecutable()
	if err != nil {
		t.Fatalf("resolve installed Codex client: %v", err)
	}
	clientProof, err := captureCodexInstalledExecutable(clientPath)
	if err != nil {
		t.Fatalf("capture installed Codex client: %v", err)
	}
	localToken, err := newCodexInstalledHTTPValidationToken()
	if err != nil {
		t.Fatal(err)
	}
	workerHandler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "normal worker used during rescue", http.StatusInternalServerError)
	})
	listener, supervisor := newCodexRuntimeSupervisorAcceptanceServer(t, workerHandler, localToken)
	listenerURL, err := url.Parse(listener.URL)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := url.Parse("https://chatgpt.com/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	fairnessKey := sha256.Sum256([]byte("installed-codex-live-rescue-acceptance"))
	relay := &RescueRelay{
		Transport: &http.Client{
			Transport: http.DefaultTransport,
			Timeout:   60 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("rescue redirect refused")
			},
		},
		Origin: origin, LoopbackHost: listenerURL.Host, ForwardingAcknowledged: true,
		DenyBearer: func(bearer []byte) bool { return string(bearer) == localToken },
		Budget:     NewRescueBudget(time.Now, fairnessKey),
	}
	if err := supervisor.ConfigureRescue(context.Background(), relay, &runtimeEvidenceTestStore{}); err != nil {
		t.Fatal(err)
	}
	enter, err := http.NewRequest(http.MethodPost, listener.URL+RuntimeRescueEnterPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	enter.Header.Set("Authorization", "Bearer "+localToken)
	enterResponse, err := http.DefaultClient.Do(enter)
	if err != nil {
		t.Fatal(err)
	}
	_ = enterResponse.Body.Close()
	if enterResponse.StatusCode != http.StatusOK {
		t.Fatalf("enter rescue = %d mode %q", enterResponse.StatusCode, supervisor.TrafficMode())
	}
	waitForRuntimeMode(t, supervisor, TrafficModeRescue)

	isolation := newCodexTaskAffinityAcceptanceIsolation(t, localToken)
	isolationAuth := filepath.Join(isolation.codexHome, "auth.json")
	if err := os.WriteFile(isolationAuth, auth, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.WriteFile(isolationAuth, nil, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("clear isolated live Codex auth: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := runCodexTaskAffinityAcceptanceTurnForTransport(ctx, codexTaskAffinityAcceptanceRunner{}, clientProof, listener.URL, isolation, false, true, "PONG"); err != nil {
		t.Fatalf("live Codex rescue turn: %v", err)
	}
}

type codexRescueAcceptanceTransport struct {
	target *url.URL
	inner  http.RoundTripper
}

func (transport *codexRescueAcceptanceTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.URL = new(url.URL)
	*clone.URL = *request.URL
	clone.URL.Scheme = transport.target.Scheme
	clone.URL.Host = transport.target.Host
	clone.Host = transport.target.Host
	return transport.inner.RoundTrip(clone)
}

type codexRuntimeSupervisorAcceptanceWorker struct {
	holder  LifecycleHolderProof
	handler http.Handler
	exited  chan struct{}
}

func (worker *codexRuntimeSupervisorAcceptanceWorker) Boot(context.Context, WorkerManifestV1) (RuntimeBootAckV1, error) {
	return RuntimeBootAckV1{SchemaVersion: 1, Kind: "runtime_boot_ack_v1", Holder: worker.holder}, nil
}

func (worker *codexRuntimeSupervisorAcceptanceWorker) BeginDrain(context.Context, TrafficMode, uint64) error {
	return nil
}

func (worker *codexRuntimeSupervisorAcceptanceWorker) AwaitQuiescence(context.Context, uint64) (RuntimeQuiescenceAckV1, error) {
	return RuntimeQuiescenceAckV1{SchemaVersion: 1, Quiescent: true}, nil
}

func (worker *codexRuntimeSupervisorAcceptanceWorker) StopAndReap(context.Context) (RuntimeWorkerReleaseV1, error) {
	return RuntimeWorkerReleaseV1{
		ProcessIdentityDigest:         "validation-process",
		ProcessTreeAbsenceProofDigest: "validation-absence",
		HolderReleaseProofDigest:      "validation-release",
	}, nil
}

func (worker *codexRuntimeSupervisorAcceptanceWorker) HolderProof() LifecycleHolderProof {
	return worker.holder
}
func (worker *codexRuntimeSupervisorAcceptanceWorker) Exited() <-chan struct{} { return worker.exited }
func (worker *codexRuntimeSupervisorAcceptanceWorker) ExecuteHTTP(context.Context, RuntimeHTTPRequestV1) (RuntimeHTTPResponseV1, error) {
	return RuntimeHTTPResponseV1{}, ErrRuntimeWorkerUnavailable
}
func (worker *codexRuntimeSupervisorAcceptanceWorker) ServeHTTP(writer http.ResponseWriter, request *http.Request, caller RuntimeCallerAuthorityV1) error {
	worker.handler.ServeHTTP(writer, request.WithContext(withRuntimeCallerAuthority(request.Context(), caller)))
	return nil
}

type codexRuntimeSupervisorAcceptanceLauncher struct {
	worker RuntimeWorkerProcess
}

func (launcher *codexRuntimeSupervisorAcceptanceLauncher) Launch(context.Context, WorkerManifestV1) (RuntimeWorkerProcess, error) {
	return launcher.worker, nil
}

type codexRuntimeSupervisorAcceptanceConsumer struct {
	mu       sync.Mutex
	consumed map[string]struct{}
}

func (consumer *codexRuntimeSupervisorAcceptanceConsumer) Consume(_ context.Context, consumption ProviderBranchAdmissionConsumptionV1) error {
	consumer.mu.Lock()
	defer consumer.mu.Unlock()
	if _, exists := consumer.consumed[consumption.AdmissionID]; exists {
		return ErrNormalCallerAdmissionReplayed
	}
	consumer.consumed[consumption.AdmissionID] = struct{}{}
	return nil
}

func newCodexRuntimeSupervisorAcceptanceServer(t *testing.T, handler http.Handler, localToken string) (*httptest.Server, *RuntimeSupervisor) {
	t.Helper()
	credential := NormalCallerCredentialV1{Domain: NormalCallerLocal, Bearer: localToken, SubjectID: "validation-local"}
	return newCodexRuntimeSupervisorAcceptanceServerWithCredential(t, handler, credential)
}

func newCodexRuntimeSupervisorAcceptanceServerWithCredential(t *testing.T, handler http.Handler, credential NormalCallerCredentialV1) (*httptest.Server, *RuntimeSupervisor) {
	t.Helper()
	events := []string{}
	worker := &codexRuntimeSupervisorAcceptanceWorker{
		holder:  runtimeHolder("validation-worker"),
		handler: normalWorkerHandler(handler, []NormalCallerCredentialV1{credential}),
		exited:  make(chan struct{}),
	}
	supervisor, err := NewRuntimeSupervisor(
		&runtimeTestListener{},
		runtimeHolder("validation-supervisor"),
		&codexRuntimeSupervisorAcceptanceLauncher{worker: worker},
		&runtimeTestCheckpointStore{events: &events},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Boot(context.Background(), WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "validation-artifact"}); err != nil {
		t.Fatal(err)
	}
	key := sha256.Sum256([]byte(credential.Bearer))
	authority, err := NewNormalCallerAuthority(
		key[:],
		1,
		[]NormalCallerCredentialV1{credential},
		&codexRuntimeSupervisorAcceptanceConsumer{consumed: make(map[string]struct{})},
		time.Now,
		rand.Reader,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.SetCallerAuthority(authority); err != nil {
		t.Fatal(err)
	}
	listener := httptest.NewServer(supervisor)
	listenerURL, err := url.Parse(listener.URL)
	if err != nil || listenerURL.Port() == "" || listenerURL.Port() == "19280" {
		listener.Close()
		t.Fatal("Codex acceptance listener did not use an isolated alternate port")
	}
	t.Cleanup(listener.Close)
	return listener, supervisor
}

func TestCodexInstalledTaskAffinityUsesHardLimitOnlyFailover(t *testing.T) {
	if os.Getenv("CQ_RUN_CODEX_TASK_AFFINITY_ACCEPTANCE") != "1" {
		t.Skip("installed Codex task-affinity acceptance requires explicit opt-in")
	}
	clientPath, err := resolveCodexInstalledClientExecutable()
	if err != nil {
		t.Skip("installed Codex client unavailable")
	}
	clientProof, err := captureCodexInstalledExecutable(clientPath)
	if err != nil {
		t.Fatalf("capture installed Codex client: %v", err)
	}
	core, err := newCodexInstalledHTTPValidationRuntimeCore(context.Background())
	if err != nil {
		t.Fatalf("open authoritative validation runtime: %v", err)
	}
	t.Cleanup(func() {
		if err := core.close(); err != nil {
			t.Errorf("close authoritative validation runtime: %v", err)
		}
	})
	localToken, err := newCodexInstalledHTTPValidationToken()
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		Config: &Config{
			LocalToken:     localToken,
			ClaudeUpstream: "http://" + core.upstream.address,
			CodexUpstream:  "http://" + core.upstream.address,
		},
		CodexNativeHTTP: core.nativeHTTPHandler(),
		Catalog:         modelregistry.NewCatalog(modelregistry.Snapshot{}),
	}
	handler, err := server.handler()
	if err != nil {
		t.Fatalf("construct candidate handler: %v", err)
	}
	listener := httptest.NewServer(handler)
	t.Cleanup(listener.Close)

	isolation := newCodexTaskAffinityAcceptanceIsolation(t, localToken)
	runner := codexTaskAffinityAcceptanceRunner{}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := runCodexTaskAffinityAcceptanceTurn(ctx, runner, clientProof, listener.URL, isolation, false, "PONG"); err != nil {
		t.Fatalf("first Codex task turn: %v", err)
	}
	if err := core.upstream.armNextNewTurnHardReplay(); err != nil {
		t.Fatalf("arm hard-limit replay: %v", err)
	}
	if err := runCodexTaskAffinityAcceptanceTurn(ctx, runner, clientProof, listener.URL, isolation, true, "PONG"); err != nil {
		t.Fatalf("second Codex task turn: %v", err)
	}
	if err := runCodexTaskAffinityAcceptanceTurn(ctx, runner, clientProof, listener.URL, isolation, true, "PONG"); err != nil {
		t.Fatalf("third Codex task turn: %v", err)
	}

	routes := core.upstream.routeHistory()
	if len(routes) != 4 {
		t.Fatalf("upstream route count = %d, want 4", len(routes))
	}
	if routes[0].AccountID != "validation-upstream-a" || routes[0].Status != 200 ||
		routes[1].AccountID != "validation-upstream-a" || routes[1].Status != 429 ||
		routes[2].AccountID != "validation-upstream-b" || routes[2].Status != 200 ||
		routes[3].AccountID != "validation-upstream-b" || routes[3].Status != 200 {
		t.Fatal("upstream route sequence did not match A/200 A/429 B/200 B/200")
	}
	sessions := []string{routes[0].Metadata.SessionID, routes[1].Metadata.SessionID, routes[2].Metadata.SessionID, routes[3].Metadata.SessionID}
	threads := []string{routes[0].Metadata.ThreadID, routes[1].Metadata.ThreadID, routes[2].Metadata.ThreadID, routes[3].Metadata.ThreadID}
	if !allCodexTaskAffinityValuesEqual(sessions) || !allCodexTaskAffinityValuesEqual(threads) {
		t.Fatal("resumed task session or thread identity changed")
	}
	turns := []string{routes[0].Metadata.TurnID, routes[1].Metadata.TurnID, routes[2].Metadata.TurnID, routes[3].Metadata.TurnID}
	if turns[0] == turns[1] || turns[1] != turns[2] || turns[2] == turns[3] {
		t.Fatal("turn identity sequence did not preserve failover retry identity")
	}
}

type codexTaskAffinityAcceptanceRunner struct{}

type codexAcceptanceDiagnosticBuffer struct {
	data []byte
}

func TestCodexAcceptanceDiagnosticsAreBoundedAndRedacted(t *testing.T) {
	buffer := &codexAcceptanceDiagnosticBuffer{}
	input := make([]byte, 20<<10)
	if written, err := buffer.Write(input); err != nil || written != len(input) || len(buffer.data) != 16<<10 {
		t.Fatalf("diagnostic write = %d/%d bytes, error %v", written, len(buffer.data), err)
	}
	diagnostic := sanitiseCodexAcceptanceDiagnostic("safe detail\nAuthorization: secret\nBearer secret\naccess_token=secret")
	if !strings.Contains(diagnostic, "safe detail") || strings.Contains(diagnostic, "secret") || strings.Count(diagnostic, "[redacted credential diagnostic]") != 3 {
		t.Fatalf("diagnostic was not safely sanitised: %q", diagnostic)
	}
}

func (buffer *codexAcceptanceDiagnosticBuffer) Write(value []byte) (int, error) {
	const limit = 16 << 10
	remaining := limit - len(buffer.data)
	if remaining > 0 {
		buffer.data = append(buffer.data, value[:min(len(value), remaining)]...)
	}
	return len(value), nil
}

func resolveCodexAcceptanceClientExecutable() (string, error) {
	path := os.Getenv("CQ_CODEX_ACCEPTANCE_EXECUTABLE")
	if path == "" {
		return resolveCodexInstalledClientExecutable()
	}
	if !filepath.IsAbs(path) {
		return "", errors.New("Codex acceptance executable must be absolute")
	}
	return path, nil
}

func sanitiseCodexAcceptanceDiagnostic(value string) string {
	lines := strings.Split(value, "\n")
	kept := lines[:0]
	for _, line := range lines {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "authorization") || strings.Contains(lower, "bearer") || strings.Contains(lower, "token") {
			kept = append(kept, "[redacted credential diagnostic]")
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

func (runner codexTaskAffinityAcceptanceRunner) Run(ctx context.Context, command codexAcceptanceCommand) ([]byte, error) {
	if runtime.GOOS != "darwin" || command.executable == "" || !command.expectedExecutable.valid() || !command.loopbackOnly {
		return nil, errors.New("Codex task-affinity runner unavailable")
	}
	before, err := captureCodexInstalledExecutable(command.executable)
	if err != nil || before != command.expectedExecutable {
		return nil, errors.New("Codex task-affinity executable changed")
	}
	profile, err := codexAcceptanceSandboxProfile(command)
	if err != nil {
		return nil, errors.New("Codex task-affinity sandbox unavailable")
	}
	arguments := append([]string{"-p", profile, command.executable}, command.args...)
	child := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", arguments...)
	child.Env = append([]string(nil), command.env...)
	child.Dir = command.dir
	child.Stdin = strings.NewReader("")
	child.Stdout = io.Discard
	stderr := &codexAcceptanceDiagnosticBuffer{}
	child.Stderr = stderr
	child.WaitDelay = 2 * time.Second
	if err := child.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("Codex task-affinity command timed out")
		}
		diagnostic := sanitiseCodexAcceptanceDiagnostic(string(stderr.data))
		if diagnostic == "" {
			return nil, fmt.Errorf("Codex task-affinity command failed: %w", err)
		}
		return nil, fmt.Errorf("Codex task-affinity command failed: %w: %s", err, diagnostic)
	}
	after, err := captureCodexInstalledExecutable(command.executable)
	if err != nil || after != command.expectedExecutable {
		return nil, errors.New("Codex task-affinity executable changed")
	}
	return nil, nil
}

type codexTaskAffinityAcceptanceIsolation struct {
	root      string
	home      string
	codexHome string
	work      string
	tmp       string
	cache     string
	config    string
	data      string
}

func newCodexTaskAffinityAcceptanceIsolation(t *testing.T, localToken string) codexTaskAffinityAcceptanceIsolation {
	t.Helper()
	shortTempRoot, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(shortTempRoot, "cq-codex-task-affinity-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	isolation := codexTaskAffinityAcceptanceIsolation{
		root:      root,
		home:      filepath.Join(root, "home"),
		codexHome: filepath.Join(root, "codex-home"),
		work:      filepath.Join(root, "work"),
		tmp:       filepath.Join(root, "tmp"),
		cache:     filepath.Join(root, "cache"),
		config:    filepath.Join(root, "config"),
		data:      filepath.Join(root, "data"),
	}
	for _, directory := range []string{isolation.home, isolation.codexHome, isolation.work, isolation.tmp, isolation.cache, isolation.config, isolation.data} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeCodexAcceptanceAuthWithToken(filepath.Join(isolation.codexHome, "auth.json"), localToken); err != nil {
		t.Fatal(err)
	}
	return isolation
}

func runCodexTaskAffinityAcceptanceTurn(
	ctx context.Context,
	runner codexAcceptanceRunner,
	client codexInstalledExecutableProof,
	baseURL string,
	isolation codexTaskAffinityAcceptanceIsolation,
	resume bool,
	output string,
) error {
	return runCodexTaskAffinityAcceptanceTurnForTransport(ctx, runner, client, baseURL, isolation, resume, false, output)
}

func runCodexTaskAffinityAcceptanceTurnForTransport(
	ctx context.Context,
	runner codexAcceptanceRunner,
	client codexInstalledExecutableProof,
	baseURL string,
	isolation codexTaskAffinityAcceptanceIsolation,
	resume bool,
	webSocket bool,
	output string,
) error {
	outputPath := filepath.Join(isolation.root, strings.ToLower(output)+".txt")
	args := codexAcceptanceExecArgumentsForTransport(baseURL, isolation.work, outputPath, webSocket)
	args = slices.DeleteFunc(args, func(value string) bool { return value == "--ephemeral" })
	args[len(args)-1] = "Reply with exactly " + output + " and no other text."
	if resume {
		args = slices.Insert(args, 1, "resume", "--last")
		args = removeCodexTaskAffinityUnsupportedResumeArguments(args)
	}
	environment := append(codexAcceptanceBaseEnvironment(isolation.home, isolation.codexHome, isolation.tmp, isolation.cache, isolation.config),
		"XDG_DATA_HOME="+isolation.data,
		"NO_PROXY=127.0.0.1,localhost",
		"no_proxy=127.0.0.1,localhost",
	)
	_, err := runner.Run(ctx, codexAcceptanceCommand{
		executable:         client.path,
		expectedExecutable: client,
		args:               args,
		env:                environment,
		dir:                isolation.work,
		endpoint:           baseURL + legacyCodexResponsesPath,
		outputPath:         outputPath,
		sandboxWriteRoot:   isolation.root,
		loopbackOnly:       true,
	})
	if err != nil {
		return err
	}
	result, err := os.ReadFile(outputPath)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(result)) != output {
		return errors.New("Codex task-affinity output mismatch")
	}
	return nil
}

func runCodexCompactionAcceptanceTurn(
	ctx context.Context,
	runner codexAcceptanceRunner,
	client codexInstalledExecutableProof,
	baseURL string,
	isolation codexTaskAffinityAcceptanceIsolation,
	resume bool,
	prompt string,
	output string,
) error {
	outputPath := filepath.Join(isolation.root, "compaction-"+strings.ToLower(output)+".txt")
	args := codexAcceptanceExecArguments(baseURL, isolation.work, outputPath)
	args = slices.DeleteFunc(args, func(value string) bool { return value == "--ephemeral" })
	args = slices.Insert(args, len(args)-1,
		"-c", "model_context_window=4096",
		"-c", "model_auto_compact_token_limit=2048",
	)
	args[len(args)-1] = prompt
	if resume {
		args = slices.Insert(args, 1, "resume", "--last")
		args = removeCodexTaskAffinityUnsupportedResumeArguments(args)
	}
	environment := append(codexAcceptanceBaseEnvironment(isolation.home, isolation.codexHome, isolation.tmp, isolation.cache, isolation.config),
		"XDG_DATA_HOME="+isolation.data,
		"NO_PROXY=127.0.0.1,localhost",
		"no_proxy=127.0.0.1,localhost",
	)
	_, err := runner.Run(ctx, codexAcceptanceCommand{
		executable:         client.path,
		expectedExecutable: client,
		args:               args,
		env:                environment,
		dir:                isolation.work,
		endpoint:           baseURL + legacyCodexResponsesPath,
		outputPath:         outputPath,
		sandboxWriteRoot:   isolation.root,
		loopbackOnly:       true,
	})
	if err != nil {
		return err
	}
	result, err := os.ReadFile(outputPath)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(result)) != output {
		return errors.New("Codex compaction output mismatch")
	}
	return nil
}

func removeCodexTaskAffinityUnsupportedResumeArguments(args []string) []string {
	result := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		if args[index] == "--color" || args[index] == "-s" || args[index] == "-C" {
			index++
			continue
		}
		result = append(result, args[index])
	}
	return result
}

func allCodexTaskAffinityValuesEqual(values []string) bool {
	return len(values) > 0 && values[0] != "" && slices.Equal(values, slices.Repeat([]string{values[0]}, len(values)))
}
