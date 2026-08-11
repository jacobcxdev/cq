package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

const (
	codexAcceptanceVersionTimeout = 3 * time.Second
	codexAcceptanceExecTimeout    = 20 * time.Second
	codexAcceptanceOutputLimit    = 4 << 10
	codexAcceptanceSandboxBase    = `(version 1) (allow default) (deny network*) (deny file-write*)`
)

type osCodexAcceptanceRunner struct{}

func (osCodexAcceptanceRunner) Run(ctx context.Context, command codexAcceptanceCommand) (result []byte, returnErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if command.executable == "" {
		return nil, errors.New("Codex acceptance executable unavailable")
	}
	executable := command.executable
	args := append([]string(nil), command.args...)
	var retainedExecutable *os.File
	executionProof := command.expectedExecutable
	var removeMaterialised func() error
	defer func() {
		if removeMaterialised != nil {
			if err := removeMaterialised(); err != nil {
				clearBytes(result)
				result = nil
				returnErr = errors.New("Codex acceptance executable cleanup failed")
			}
		}
	}()
	if command.expectedExecutable != (codexInstalledExecutableProof{}) {
		if !command.loopbackOnly || executable != command.expectedExecutable.path || !command.expectedExecutable.valid() {
			return nil, errors.New("Codex acceptance executable identity unavailable")
		}
		var err error
		retainedExecutable, err = openCodexInstalledAcceptanceExecutable(command.expectedExecutable)
		if err != nil {
			return nil, errors.New("Codex acceptance executable identity unavailable")
		}
		defer retainedExecutable.Close()
		if command.loopbackOnly {
			executionProof, removeMaterialised, err = materialiseCodexAcceptanceExecutable(command.sandboxWriteRoot, command.expectedExecutable, retainedExecutable)
			if err != nil {
				return nil, errors.New("Codex acceptance executable isolation unavailable")
			}
			executable = executionProof.path
		}
	}
	if command.loopbackOnly {
		if runtime.GOOS != "darwin" {
			return nil, errors.New("Codex acceptance network confinement unavailable")
		}
		if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
			return nil, errors.New("Codex acceptance network confinement unavailable")
		}
		profile, err := codexAcceptanceSandboxProfile(command)
		if err != nil {
			return nil, errors.New("Codex acceptance sandbox authority unavailable")
		}
		args = append([]string{"-p", profile, executable}, args...)
		executable = "/usr/bin/sandbox-exec"
	}

	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Env = append([]string(nil), command.env...)
	cmd.Dir = command.dir
	cmd.Stdin = strings.NewReader("")
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 2 * time.Second
	var output codexAcceptanceLimitedBuffer
	output.limit = codexAcceptanceOutputLimit
	if command.captureOutput {
		cmd.Stdout = &output
	} else {
		cmd.Stdout = io.Discard
	}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("Codex acceptance command timed out")
		}
		return nil, errors.New("Codex acceptance command failed")
	}
	if retainedExecutable != nil {
		current, err := captureCodexInstalledExecutable(command.expectedExecutable.path)
		if err != nil || current != command.expectedExecutable {
			return nil, errors.New("Codex acceptance executable identity changed")
		}
		if command.loopbackOnly {
			currentExecution, err := captureCodexInstalledExecutable(executionProof.path)
			if err != nil || currentExecution != executionProof {
				return nil, errors.New("Codex acceptance isolated executable identity changed")
			}
		}
	}
	return output.Bytes(), nil
}

func materialiseCodexAcceptanceExecutable(
	root string,
	expected codexInstalledExecutableProof,
	retained *os.File,
) (codexInstalledExecutableProof, func() error, error) {
	cleanRoot := filepath.Clean(root)
	resolvedRoot, err := filepath.EvalSymlinks(cleanRoot)
	if err != nil || resolvedRoot != cleanRoot || !filepath.IsAbs(cleanRoot) || retained == nil || !expected.valid() {
		return codexInstalledExecutableProof{}, nil, errCodexInstalledProcessAttestation
	}
	executionRoot, err := os.MkdirTemp(filepath.Dir(cleanRoot), ".cq-codex-installed-exec-")
	if err != nil {
		return codexInstalledExecutableProof{}, nil, errCodexInstalledProcessAttestation
	}
	removeExecutionRoot := true
	defer func() {
		if removeExecutionRoot {
			_ = os.Remove(executionRoot)
		}
	}()
	resolvedExecutionRoot, err := filepath.EvalSymlinks(executionRoot)
	if err != nil || resolvedExecutionRoot != executionRoot {
		return codexInstalledExecutableProof{}, nil, errCodexInstalledProcessAttestation
	}
	fsys := fsutil.OSFileSystem{}
	directory, err := fsys.OpenSecureDirectory(executionRoot)
	if err != nil {
		return codexInstalledExecutableProof{}, nil, errCodexInstalledProcessAttestation
	}
	closeDirectory := true
	defer func() {
		if closeDirectory {
			_ = directory.Close()
		}
	}()
	if err := fsutil.ValidateSecureDirectoryHandle(fsys, directory, executionRoot); err != nil {
		return codexInstalledExecutableProof{}, nil, errCodexInstalledProcessAttestation
	}
	const name = ".cq-installed-client"
	file, err := directory.CreateExclusive(name, 0o500)
	if err != nil {
		return codexInstalledExecutableProof{}, nil, errCodexInstalledProcessAttestation
	}
	fileClosed := false
	defer func() {
		if !fileClosed {
			_ = file.Close()
		}
	}()
	removeFile := true
	defer func() {
		if removeFile {
			_ = directory.Remove(name)
		}
	}()
	if _, err := retained.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return codexInstalledExecutableProof{}, nil, errCodexInstalledProcessAttestation
	}
	written, copyErr := io.Copy(file, io.LimitReader(retained, expected.size+1))
	if copyErr != nil || written != expected.size {
		return codexInstalledExecutableProof{}, nil, errCodexInstalledProcessAttestation
	}
	if err := file.Sync(); err != nil {
		return codexInstalledExecutableProof{}, nil, errCodexInstalledProcessAttestation
	}
	if err := file.Close(); err != nil {
		fileClosed = true
		return codexInstalledExecutableProof{}, nil, errCodexInstalledProcessAttestation
	}
	fileClosed = true
	if err := directory.Sync(); err != nil {
		return codexInstalledExecutableProof{}, nil, errCodexInstalledProcessAttestation
	}
	if err := fsutil.ValidateSecureDirectoryHandle(fsys, directory, executionRoot); err != nil {
		return codexInstalledExecutableProof{}, nil, errCodexInstalledProcessAttestation
	}
	executionProof, err := captureCodexInstalledExecutable(filepath.Join(executionRoot, name))
	if err != nil || executionProof.sha256 != expected.sha256 || executionProof.size != expected.size {
		return codexInstalledExecutableProof{}, nil, errCodexInstalledProcessAttestation
	}
	removeFile = false
	closeDirectory = false
	removeExecutionRoot = false
	cleanup := func() error {
		if err := fsutil.ValidateSecureDirectoryHandle(fsys, directory, executionRoot); err != nil {
			_ = directory.Close()
			return errCodexInstalledProcessAttestation
		}
		removeErr := directory.Remove(name)
		syncErr := directory.Sync()
		closeErr := directory.Close()
		rootRemoveErr := os.Remove(executionRoot)
		return errors.Join(removeErr, syncErr, closeErr, rootRemoveErr)
	}
	return executionProof, cleanup, nil
}

func codexAcceptanceSandboxProfile(command codexAcceptanceCommand) (string, error) {
	profile := codexAcceptanceSandboxBase
	seen := make(map[string]bool, 2)
	for _, rawURL := range []string{command.endpoint, command.egressProxyURL} {
		if rawURL == "" {
			continue
		}
		address, err := codexAcceptanceSandboxLoopbackAddress(rawURL)
		if err != nil {
			return "", err
		}
		if !seen[address] {
			profile += ` (allow network-outbound (remote ip ` + strconv.Quote(address) + `))`
			seen[address] = true
		}
	}
	if command.sandboxWriteRoot != "" {
		root := filepath.Clean(command.sandboxWriteRoot)
		if !filepath.IsAbs(root) || root == string(filepath.Separator) || filepath.Dir(root) == root {
			return "", errCodexInstalledListenerAcceptance
		}
		profile += ` (allow file-write* (subpath ` + strconv.Quote(root) + `))`
	}
	return profile, nil
}

func codexAcceptanceSandboxLoopbackAddress(rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.Hostname() != "127.0.0.1" || parsed.Port() == "" {
		return "", errCodexInstalledListenerAcceptance
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return "", errCodexInstalledListenerAcceptance
	}
	return net.JoinHostPort("localhost", strconv.Itoa(port)), nil
}

func openCodexInstalledAcceptanceExecutable(expected codexInstalledExecutableProof) (*os.File, error) {
	if !expected.valid() {
		return nil, errCodexInstalledProcessAttestation
	}
	inspector := fsutil.OSFileSystem{}
	opened, err := inspector.OpenNoFollow(expected.path)
	if err != nil {
		return nil, errCodexInstalledProcessAttestation
	}
	file, ok := opened.(*os.File)
	if !ok {
		_ = opened.Close()
		return nil, errCodexInstalledProcessAttestation
	}
	succeeded := false
	defer func() {
		if !succeeded {
			_ = file.Close()
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, errCodexInstalledProcessAttestation
	}
	owner, ownerOK := inspector.FileOwnerUID(info)
	identity, identityOK := inspector.FileIdentity(info)
	if !ownerOK || !identityOK || identity.Device != expected.device || identity.Inode != expected.inode ||
		identity.Links != expected.links || owner != expected.owner || info.Size() != expected.size || info.Mode() != expected.mode {
		return nil, errCodexInstalledProcessAttestation
	}
	hash := sha256.New()
	if copied, err := io.Copy(hash, io.LimitReader(file, codexInstalledExecutableMaxBytes+1)); err != nil || copied != expected.size {
		return nil, errCodexInstalledProcessAttestation
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	if digest != expected.sha256 {
		return nil, errCodexInstalledProcessAttestation
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, errCodexInstalledProcessAttestation
	}
	after, err := file.Stat()
	if err != nil {
		return nil, errCodexInstalledProcessAttestation
	}
	afterIdentity, afterIdentityOK := inspector.FileIdentity(after)
	if !afterIdentityOK || afterIdentity != identity || after.Size() != info.Size() || after.Mode() != info.Mode() {
		return nil, errCodexInstalledProcessAttestation
	}
	succeeded = true
	return file, nil
}

type codexAcceptanceLimitedBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (buffer *codexAcceptanceLimitedBuffer) Write(data []byte) (int, error) {
	written := len(data)
	remaining := buffer.limit - buffer.buffer.Len()
	if remaining > 0 {
		_, _ = buffer.buffer.Write(data[:min(len(data), remaining)])
	}
	return written, nil
}

func (buffer *codexAcceptanceLimitedBuffer) Bytes() []byte {
	return append([]byte(nil), buffer.buffer.Bytes()...)
}

type codexInstalledAcceptanceTraffic struct {
	requests      atomic.Uint64
	modelRequests atomic.Uint64
	unexpected    atomic.Uint64

	mu               sync.Mutex
	requestBody      []byte
	requestEncoding  string
	requestMetadata  string
	requestToken     string
	requestAccount   string
	requestAPIKey    string
	upstreamBody     []byte
	upstreamEncoding string
	upstreamToken    string
	upstreamAccount  string
	upstreamAPIKey   string
}

func (traffic *codexInstalledAcceptanceTraffic) guard(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/models" && request.URL.RawQuery == "client_version=0.146.0" {
			traffic.modelRequests.Add(1)
			writer.Header().Set("Content-Type", "application/json")
			writer.Header().Set("ETag", `"cq-acceptance-models-0.146"`)
			_, _ = io.WriteString(writer, `{"models":[]}`)
			return
		}
		if request.Method != http.MethodPost || request.URL.Path != legacyCodexResponsesPath || request.URL.RawQuery != "" {
			traffic.unexpected.Add(1)
			http.NotFound(writer, request)
			return
		}
		body, err := io.ReadAll(io.LimitReader(request.Body, maxRequestBody+1))
		_ = request.Body.Close()
		if err != nil {
			http.Error(writer, "invalid acceptance request", http.StatusBadRequest)
			return
		}
		request.Body = io.NopCloser(bytes.NewReader(body))
		request.ContentLength = int64(len(body))
		traffic.requests.Add(1)
		traffic.mu.Lock()
		traffic.requestBody = append([]byte(nil), body...)
		traffic.requestEncoding = request.Header.Get("Content-Encoding")
		traffic.requestMetadata = request.Header.Get(codexTurnMetadataKey)
		traffic.requestToken = strings.TrimSpace(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "))
		traffic.requestAccount = request.Header.Get("ChatGPT-Account-ID")
		traffic.requestAPIKey = request.Header.Get("x-api-key")
		traffic.mu.Unlock()
		handler.ServeHTTP(writer, request)
	})
}

func (traffic *codexInstalledAcceptanceTraffic) serveUpstream(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost || request.URL.Path != legacyCodexResponsesPath || request.URL.RawQuery != "" {
		traffic.unexpected.Add(1)
		http.NotFound(writer, request)
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxRequestBody+1))
	_ = request.Body.Close()
	if err != nil {
		http.Error(writer, "invalid acceptance request", http.StatusBadRequest)
		return
	}
	traffic.mu.Lock()
	traffic.upstreamBody = append([]byte(nil), body...)
	traffic.upstreamEncoding = request.Header.Get("Content-Encoding")
	traffic.upstreamToken = strings.TrimSpace(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "))
	traffic.upstreamAccount = request.Header.Get("ChatGPT-Account-ID")
	traffic.upstreamAPIKey = request.Header.Get("x-api-key")
	traffic.mu.Unlock()

	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	_, _ = io.WriteString(writer, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"acceptance-response\"}}\n\n")
	_, _ = io.WriteString(writer, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"acceptance-message\",\"content\":[{\"type\":\"output_text\",\"text\":\"PONG\"}]}}\n\n")
	_, _ = io.WriteString(writer, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"acceptance-response\",\"end_turn\":true,\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
}

type codexInstalledAcceptanceTrafficSnapshot struct {
	requestBody      []byte
	requestEncoding  string
	requestMetadata  string
	requestToken     string
	requestAccount   string
	requestAPIKey    string
	upstreamBody     []byte
	upstreamEncoding string
	upstreamToken    string
	upstreamAccount  string
	upstreamAPIKey   string
}

func (traffic *codexInstalledAcceptanceTraffic) snapshot() codexInstalledAcceptanceTrafficSnapshot {
	traffic.mu.Lock()
	defer traffic.mu.Unlock()
	return codexInstalledAcceptanceTrafficSnapshot{
		requestBody:      append([]byte(nil), traffic.requestBody...),
		requestEncoding:  traffic.requestEncoding,
		requestMetadata:  traffic.requestMetadata,
		requestToken:     traffic.requestToken,
		requestAccount:   traffic.requestAccount,
		requestAPIKey:    traffic.requestAPIKey,
		upstreamBody:     append([]byte(nil), traffic.upstreamBody...),
		upstreamEncoding: traffic.upstreamEncoding,
		upstreamToken:    traffic.upstreamToken,
		upstreamAccount:  traffic.upstreamAccount,
		upstreamAPIKey:   traffic.upstreamAPIKey,
	}
}

type codexAcceptanceHeadroomMonitor struct {
	requests    atomic.Uint64
	parseErrors atomic.Uint64
	done        chan struct{}
}

func newCodexAcceptanceHeadroom() (*HeadroomBridge, *codexAcceptanceHeadroomMonitor, func()) {
	requestReader, requestWriter := io.Pipe()
	responseReader, responseWriter := io.Pipe()
	responseScanner := bufio.NewScanner(responseReader)
	responseScanner.Buffer(make([]byte, 0, 64<<10), maxRequestBody)
	monitor := &codexAcceptanceHeadroomMonitor{done: make(chan struct{})}
	go func() {
		defer close(monitor.done)
		defer requestReader.Close()
		defer responseWriter.Close()
		scanner := bufio.NewScanner(requestReader)
		scanner.Buffer(make([]byte, 0, 64<<10), maxRequestBody)
		for scanner.Scan() {
			monitor.requests.Add(1)
			var request headroomResponsesRequest
			if err := json.Unmarshal(scanner.Bytes(), &request); err != nil ||
				request.Operation != "compress_responses" || request.Model == "" ||
				len(request.Input) == 0 || !json.Valid(request.Input) {
				monitor.parseErrors.Add(1)
			}
			_, _ = io.WriteString(responseWriter, "{\"ok\":false,\"reason\":\"acceptance_skip\",\"input\":null,\"instructions\":null,\"clear_instructions\":false,\"tokens_saved\":0,\"compression_ratio\":1}\n")
		}
		if err := scanner.Err(); err != nil {
			monitor.parseErrors.Add(1)
		}
	}()
	bridge := &HeadroomBridge{stdin: requestWriter, stdout: responseScanner}
	closeBridge := func() {
		_ = requestWriter.Close()
		<-monitor.done
		_ = responseReader.Close()
	}
	return bridge, monitor, closeBridge
}

func runCodexHTTPInstalledAcceptance(ctx context.Context, dependencies codexAcceptanceDependencies) (CodexHTTPAcceptanceResult, error) {
	result, err := runCodexHTTPEnforcedAcceptance(ctx)
	if err != nil {
		return result, err
	}
	installed, err := runCodexInstalledClientAcceptance(ctx, dependencies)
	mergeCodexInstalledAcceptanceResult(&result, installed)
	return result, err
}

// RunCodexHTTPInstalledAcceptance preserves the deterministic 20-turn HTTP
// corpus and adds one isolated request from the installed Codex 0.146 client.
// Both proxy listeners, credentials, auth files, and upstreams are synthetic.
func RunCodexHTTPInstalledAcceptance(ctx context.Context) (CodexHTTPAcceptanceResult, error) {
	return runCodexHTTPInstalledAcceptance(ctx, codexAcceptanceDependencies{
		executable: codexAcceptanceExecutable,
		runner:     osCodexAcceptanceRunner{},
	})
}

func mergeCodexInstalledAcceptanceResult(result *CodexHTTPAcceptanceResult, installed CodexHTTPAcceptanceResult) {
	result.InstalledVersion = installed.InstalledVersion
	result.InstalledRequests = installed.InstalledRequests
	result.InstalledModelRequests = installed.InstalledModelRequests
	result.InstalledAttempts = installed.InstalledAttempts
	result.InstalledSelectorCalls = installed.InstalledSelectorCalls
	result.InstalledStrongKeys = installed.InstalledStrongKeys
	result.InstalledZstdRequests = installed.InstalledZstdRequests
	result.InstalledUnknownEvents = installed.InstalledUnknownEvents
	result.InstalledContinuityErrors = installed.InstalledContinuityErrors
	result.InstalledQuiescentLeases = installed.InstalledQuiescentLeases
	result.HeadroomRequests = installed.HeadroomRequests
	result.HeadroomParseErrors = installed.HeadroomParseErrors
	result.UnexpectedRoutes = installed.UnexpectedRoutes
	result.EgressAttempts = installed.EgressAttempts
	result.InstalledResolutions = installed.InstalledResolutions
	result.PongVerified = installed.PongVerified
}

func runCodexInstalledClientAcceptance(ctx context.Context, dependencies codexAcceptanceDependencies) (result CodexHTTPAcceptanceResult, returnErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if dependencies.executable == "" {
		dependencies.executable = codexAcceptanceExecutable
	}
	if dependencies.runner == nil {
		dependencies.runner = osCodexAcceptanceRunner{}
	}

	versionCtx, cancelVersion := context.WithTimeout(ctx, codexAcceptanceVersionTimeout)
	versionOutput, err := dependencies.runner.Run(versionCtx, codexAcceptanceCommand{
		executable:    dependencies.executable,
		args:          []string{"--version"},
		env:           codexAcceptanceBaseEnvironment("", "", "", "", ""),
		captureOutput: true,
		loopbackOnly:  true,
	})
	cancelVersion()
	if err != nil {
		return result, errors.New("installed Codex acceptance version probe failed")
	}
	if !bytes.Equal(versionOutput, []byte(codexAcceptanceVersion)) && !bytes.Equal(versionOutput, []byte(codexAcceptanceVersion+"\n")) {
		return result, errors.New("installed Codex acceptance version mismatch")
	}
	result.InstalledVersion = codexAcceptanceVersion

	root, err := os.MkdirTemp("", "cq-codex-installed-acceptance-")
	if err != nil {
		return result, errors.New("create installed Codex acceptance isolation")
	}
	defer func() {
		if err := os.RemoveAll(root); err != nil && returnErr == nil {
			returnErr = errors.New("remove installed Codex acceptance isolation")
		}
	}()
	home := filepath.Join(root, "home")
	codexHome := filepath.Join(root, "codex-home")
	work := filepath.Join(root, "work")
	tmp := filepath.Join(root, "tmp")
	cache := filepath.Join(root, "cache")
	config := filepath.Join(root, "config")
	data := filepath.Join(root, "data")
	for _, directory := range []string{home, codexHome, work, tmp, cache, config, data} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return result, errors.New("prepare installed Codex acceptance isolation")
		}
	}
	if err := writeCodexAcceptanceAuth(filepath.Join(codexHome, "auth.json")); err != nil {
		return result, errors.New("prepare installed Codex acceptance auth")
	}

	traffic := &codexInstalledAcceptanceTraffic{}
	upstreamListener, upstreamServer, upstreamErrors, err := startCodexAcceptanceHTTP(http.HandlerFunc(traffic.serveUpstream))
	if err != nil {
		return result, errors.New("start installed Codex acceptance upstream")
	}
	defer shutdownCodexAcceptanceServer(upstreamServer)

	egressAttempts := &atomic.Uint64{}
	egressListener, egressServer, egressErrors, err := startCodexAcceptanceHTTP(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		egressAttempts.Add(1)
		http.Error(writer, "acceptance egress denied", http.StatusBadGateway)
	}))
	if err != nil {
		return result, errors.New("start installed Codex acceptance egress guard")
	}
	defer shutdownCodexAcceptanceServer(egressServer)

	credentials := newCodexAcceptanceCredentials()
	chooser := &codexAcceptanceChooser{}
	router := &CodexRequestRouter{
		Scope: &CodexRequestScope{Chooser: chooser, Inventory: credentials},
		Executor: &CodexAttemptExecutor{
			Inventory: credentials,
			Secrets:   credentials,
			Transport: &CodexTokenTransport{Inner: &http.Transport{
				Proxy:             nil,
				ForceAttemptHTTP2: false,
			}},
		},
	}
	store, err := OpenCodexLeaseStore(fsutil.NewMemFS(), "/installed-acceptance/leases.json", "/installed-acceptance/leases.key")
	if err != nil {
		return result, errors.New("prepare installed Codex acceptance lease store")
	}
	observer, err := NewCodexTurnObserver(NewCodexTurnLeaseManager(1, false, nil), store)
	if err != nil {
		return result, errors.New("prepare installed Codex acceptance observer")
	}
	headroom, headroomMonitor, closeHeadroom := newCodexAcceptanceHeadroom()
	defer closeHeadroom()

	upstreamURL := "http://" + upstreamListener.Addr().String()
	server := &Server{
		Config: &Config{
			LocalToken:     codexAcceptanceLocalToken,
			ClaudeUpstream: upstreamURL,
			CodexUpstream:  upstreamURL,
		},
		CodexRequests: router,
		CodexObserver: observer,
		Headroom:      headroom,
		HeadroomMode:  HeadroomModeToken,
	}
	handler, err := server.handler()
	if err != nil {
		return result, errors.New("prepare installed Codex acceptance proxy")
	}
	proxyListener, proxyServer, proxyErrors, err := startCodexAcceptanceHTTP(traffic.guard(handler))
	if err != nil {
		return result, errors.New("start installed Codex acceptance proxy")
	}
	defer shutdownCodexAcceptanceServer(proxyServer)

	proxyBaseURL := "http://" + proxyListener.Addr().String()
	egressURL := "http://" + egressListener.Addr().String()
	outputPath := filepath.Join(root, "last-message.txt")
	environment := codexAcceptanceBaseEnvironment(home, codexHome, tmp, cache, config)
	environment = append(environment,
		"XDG_DATA_HOME="+data,
		"HTTP_PROXY="+egressURL,
		"HTTPS_PROXY="+egressURL,
		"ALL_PROXY="+egressURL,
		"http_proxy="+egressURL,
		"https_proxy="+egressURL,
		"all_proxy="+egressURL,
		"NO_PROXY=127.0.0.1,localhost",
		"no_proxy=127.0.0.1,localhost",
	)
	command := codexAcceptanceCommand{
		executable:     dependencies.executable,
		args:           codexAcceptanceExecArguments(proxyBaseURL, work, outputPath),
		env:            environment,
		dir:            work,
		endpoint:       proxyBaseURL + legacyCodexResponsesPath,
		outputPath:     outputPath,
		egressProxyURL: egressURL,
		loopbackOnly:   true,
	}
	execCtx, cancelExec := context.WithTimeout(ctx, codexAcceptanceExecTimeout)
	_, err = dependencies.runner.Run(execCtx, command)
	cancelExec()
	if err != nil {
		return result, errors.New("installed Codex acceptance request failed")
	}

	output, err := readCodexAcceptanceOutput(outputPath)
	if err != nil {
		return result, errors.New("read installed Codex acceptance output")
	}
	result.PongVerified = bytes.Equal(output, []byte("PONG")) || bytes.Equal(output, []byte("PONG\n"))
	health := observer.Health()
	result.InstalledRequests = health.Requests
	result.InstalledModelRequests = traffic.modelRequests.Load()
	result.InstalledAttempts = health.Attempts
	result.InstalledSelectorCalls = chooser.Calls()
	result.InstalledStrongKeys = health.StrongKeys
	result.InstalledZstdRequests = health.ZstdRequests
	result.InstalledUnknownEvents = health.Unknown
	result.InstalledContinuityErrors = health.ContinuityErrors
	result.InstalledQuiescentLeases = health.Leases[LeaseBoundQuiescent.String()]
	result.HeadroomRequests = headroomMonitor.requests.Load()
	result.HeadroomParseErrors = headroomMonitor.parseErrors.Load()
	result.UnexpectedRoutes = traffic.unexpected.Load()
	result.EgressAttempts = egressAttempts.Load()
	result.InstalledResolutions = credentials.Resolutions()

	if err := validateCodexInstalledAcceptanceEvidence(result, health, observer, traffic); err != nil {
		return result, err
	}
	for _, serverErrors := range []<-chan error{upstreamErrors, egressErrors, proxyErrors} {
		if err := codexAcceptanceServeError(serverErrors); err != nil {
			return result, errors.New("installed Codex acceptance listener failed")
		}
	}
	return result, nil
}

func startCodexAcceptanceHTTP(handler http.Handler) (net.Listener, *http.Server, <-chan error, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, nil, err
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	errors := make(chan error, 1)
	go func() {
		errors <- server.Serve(listener)
	}()
	return listener, server, errors, nil
}

func codexAcceptanceBaseEnvironment(home, codexHome, tmp, cache, config string) []string {
	environment := []string{
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin",
		"LANG=C",
		"LC_ALL=C",
		"TERM=dumb",
		"NO_COLOR=1",
		"CI=1",
		"SHELL=/bin/zsh",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
	}
	for _, entry := range []struct{ key, value string }{
		{"HOME", home},
		{"CODEX_HOME", codexHome},
		{"TMPDIR", tmp},
		{"XDG_CACHE_HOME", cache},
		{"XDG_CONFIG_HOME", config},
	} {
		if entry.value != "" {
			environment = append(environment, entry.key+"="+entry.value)
		}
	}
	return environment
}

func codexAcceptanceExecArguments(baseURL, work, outputPath string) []string {
	provider := "model_providers.cq_acceptance."
	return []string{
		"exec",
		"--ephemeral",
		"--ignore-user-config",
		"--ignore-rules",
		"--skip-git-repo-check",
		"--strict-config",
		"--color", "never",
		"-s", "read-only",
		"-C", work,
		"-o", outputPath,
		"--model", "gpt-5.6-sol",
		"-c", `model_provider="cq_acceptance"`,
		"-c", provider + `name = "OpenAI"`,
		"-c", provider + "base_url = " + strconv.Quote(baseURL),
		"-c", provider + `wire_api="responses"`,
		"-c", provider + `requires_openai_auth=true`,
		"-c", provider + `supports_websockets = false`,
		"-c", provider + `supports_standalone_web_search=false`,
		"-c", provider + `request_max_retries = 0`,
		"-c", provider + `stream_max_retries = 0`,
		"-c", `approval_policy="never"`,
		"-c", `analytics.enabled=false`,
		"-c", `cli_auth_credentials_store="file"`,
		"-c", `check_for_update_on_startup=false`,
		"-c", `features.enable_request_compression=true`,
		"-c", `features.plugins=false`,
		"-c", `features.respect_system_proxy=true`,
		"Reply with exactly PONG and no other text.",
	}
}

func writeCodexAcceptanceAuth(path string) error {
	return writeCodexAcceptanceAuthWithToken(path, codexAcceptanceLocalToken)
}

func writeCodexAcceptanceAuthWithToken(path, accessToken string) error {
	if strings.TrimSpace(accessToken) == "" {
		return errCodexInstalledListenerAcceptance
	}
	jwt := codexAcceptanceJWT()
	auth := map[string]any{
		"auth_mode":      "chatgpt",
		"OPENAI_API_KEY": nil,
		"tokens": map[string]any{
			"id_token":      jwt,
			"access_token":  accessToken,
			"refresh_token": "acceptance-refresh",
			"account_id":    "acceptance-bootstrap",
		},
		"last_refresh": time.Now().UTC().Format(time.RFC3339Nano),
	}
	data, err := json.Marshal(auth)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func codexAcceptanceJWT() string {
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	payload, _ := json.Marshal(map[string]any{
		"email": "cq-acceptance@invalid.example",
		"exp":   time.Now().Add(time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_plan_type":  "pro",
			"chatgpt_user_id":    "acceptance-bootstrap-user",
			"chatgpt_account_id": "acceptance-bootstrap",
		},
	})
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func readCodexAcceptanceOutput(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 6))
	if err != nil {
		return nil, err
	}
	if len(data) > 5 {
		return nil, errors.New("installed Codex acceptance output too long")
	}
	return data, nil
}

func validateCodexInstalledAcceptanceEvidence(result CodexHTTPAcceptanceResult, health CodexObservationHealth, observer *CodexTurnObserver, traffic *codexInstalledAcceptanceTraffic) error {
	snapshot := traffic.snapshot()
	// Codex 0.146 has two concurrent model-catalogue consumers during startup.
	// Depending on scheduling they coalesce into one GET or issue two identical
	// GETs. The exact route guard above keeps both local and version-specific.
	if result.InstalledVersion != codexAcceptanceVersion || result.InstalledRequests != 1 ||
		result.InstalledModelRequests < 1 || result.InstalledModelRequests > 2 ||
		result.InstalledAttempts != 1 || result.InstalledSelectorCalls != 1 ||
		result.InstalledStrongKeys != 1 || result.InstalledZstdRequests != 1 ||
		result.InstalledUnknownEvents != 0 || result.InstalledContinuityErrors != 0 ||
		result.InstalledQuiescentLeases != 1 || len(health.Leases) != 1 ||
		result.HeadroomRequests != 1 || result.HeadroomParseErrors != 0 ||
		result.UnexpectedRoutes != 0 || result.EgressAttempts != 0 ||
		result.InstalledResolutions != 1 || !result.PongVerified ||
		traffic.requests.Load() != 1 || health.MetadataHeaders != 1 ||
		health.RequestDecodeErrors != 0 || health.MetadataParseErrors != 0 ||
		health.MissingTurnIdentity != 0 || health.Failovers != 0 {
		return errors.New("installed Codex acceptance evidence incomplete")
	}
	if !strings.EqualFold(snapshot.requestEncoding, "zstd") ||
		!strings.EqualFold(snapshot.upstreamEncoding, "zstd") ||
		snapshot.requestMetadata == "" || snapshot.requestToken != codexAcceptanceLocalToken ||
		snapshot.requestAccount != "acceptance-bootstrap" || snapshot.requestAPIKey != "" ||
		!bytes.Equal(snapshot.requestBody, snapshot.upstreamBody) ||
		snapshot.upstreamToken != "cq-token-acceptance-one" ||
		snapshot.upstreamAccount != "acceptance-one" || snapshot.upstreamAPIKey != "" {
		return errors.New("installed Codex acceptance routing evidence incomplete")
	}
	metadata, err := ParseCodexTurnMetadata(nil, snapshot.requestMetadata, nil)
	leases := observer.Leases.Snapshot()
	if err != nil || !metadata.Found || !metadata.Strong || len(leases) != 1 ||
		leases[0].Key != NewCodexLeaseKey(metadata.Metadata) || leases[0].State != LeaseBoundQuiescent ||
		leases[0].AccountKey != "acceptance-one" || leases[0].RoutingRefs != 0 || leases[0].ActiveAttempts != 0 {
		return errors.New("installed Codex acceptance lease evidence incomplete")
	}
	decoded, err := DecodeCodexRequest(snapshot.requestBody, snapshot.requestEncoding, DefaultCodexZstdLimits)
	if err != nil || !json.Valid(decoded.Decoded()) || extractModel(decoded.Decoded()) != "gpt-5.6-sol" {
		return errors.New("installed Codex acceptance request evidence invalid")
	}
	return nil
}
