package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

var (
	ErrCodexValidationClientUnavailable = errors.New("Codex validation client unavailable")
	ErrCodexValidationBuildMismatch     = errors.New("Codex validation client build mismatch")
)

type codexInstalledWebSocketValidationDependencies struct {
	cleanupContext    context.Context
	resolveExecutable func() (string, error)
	captureExecutable func(string) (codexInstalledExecutableProof, error)
	runVersion        func(context.Context, string, codexInstalledExecutableProof) ([]byte, error)
	runner            codexAcceptanceRunner
	now               func() time.Time
}

// RunCodexInstalledWebSocketValidation validates one isolated candidate
// listener with exact installed Codex CLI. It never inspects, stops, replaces,
// or restarts configured proxy service.
func RunCodexInstalledWebSocketValidation(ctx context.Context, cqBuild, clientBuild, clientExecutable, markerDir string) (CodexReadinessMarker, error) {
	return runCodexInstalledWebSocketValidation(ctx, nil, cqBuild, clientBuild, clientExecutable, markerDir)
}

// RunCodexInstalledWebSocketValidationWithCleanup uses the caller's reserved
// cleanup context; neither preparation nor cleanup starts a new allowance.
func RunCodexInstalledWebSocketValidationWithCleanup(ctx, cleanup context.Context, cqBuild, clientBuild, clientExecutable, markerDir string) (CodexReadinessMarker, error) {
	if cleanup == nil {
		return CodexReadinessMarker{}, errCodexInstalledListenerAcceptance
	}
	return runCodexInstalledWebSocketValidation(ctx, cleanup, cqBuild, clientBuild, clientExecutable, markerDir)
}

func runCodexInstalledWebSocketValidation(ctx, cleanup context.Context, cqBuild, clientBuild, clientExecutable, markerDir string) (CodexReadinessMarker, error) {
	if strings.TrimSpace(markerDir) == "" {
		paths, err := ResolveDefaultPaths(userdirs.StateRoot)
		if err != nil {
			return CodexReadinessMarker{}, err
		}
		markerDir = paths.StateDir
	}
	resolveExecutable := resolveCodexInstalledClientExecutable
	if strings.TrimSpace(clientExecutable) != "" {
		resolveExecutable = func() (string, error) { return clientExecutable, nil }
	}
	return runCodexInstalledWebSocketValidationWithDependencies(ctx, cqBuild, clientBuild, markerDir, codexInstalledWebSocketValidationDependencies{
		cleanupContext:    cleanup,
		resolveExecutable: resolveExecutable,
		captureExecutable: captureCodexInstalledExecutable,
		runVersion: func(ctx context.Context, path string, proof codexInstalledExecutableProof) ([]byte, error) {
			return runCodexInstalledVersionCommandWithCleanup(ctx, cleanup, path, proof, osCodexAcceptanceRunner{})
		},
		runner: osCodexAcceptanceRunner{},
		now:    time.Now,
	})
}

func runCodexInstalledWebSocketValidationWithDependencies(
	ctx context.Context,
	cqBuild string,
	clientBuild string,
	markerDir string,
	dependencies codexInstalledWebSocketValidationDependencies,
) (marker CodexReadinessMarker, returnErr error) {
	if strings.TrimSpace(markerDir) == "" {
		return marker, errCodexInstalledListenerAcceptance
	}
	markerDir = filepath.Clean(markerDir)
	if !filepath.IsAbs(markerDir) {
		return marker, errCodexInstalledListenerAcceptance
	}
	defer func() {
		if recover() != nil {
			returnErr = errCodexInstalledListenerAcceptance
		}
		if ctx != nil && ctx.Err() != nil {
			returnErr = errors.Join(returnErr, ctx.Err())
		}
		if dependencies.cleanupContext != nil && dependencies.cleanupContext.Err() != nil {
			returnErr = errors.Join(returnErr, dependencies.cleanupContext.Err())
		}
		if returnErr != nil {
			marker = CodexReadinessMarker{}
			returnErr = errors.Join(returnErr, invalidateCodexWebSocketReadinessMarkerDurably(markerDir))
		}
	}()
	if err := invalidateCodexWebSocketReadinessMarkerDurably(markerDir); err != nil {
		return marker, err
	}
	if ctx == nil || ctx.Err() != nil || strings.TrimSpace(cqBuild) == "" ||
		clientBuild != strings.TrimSpace(clientBuild) || !codexInstalledHTTPClientBuildPattern.MatchString(clientBuild) ||
		dependencies.resolveExecutable == nil || dependencies.captureExecutable == nil || dependencies.runVersion == nil || dependencies.runner == nil {
		return marker, errCodexInstalledListenerAcceptance
	}
	if dependencies.now == nil {
		dependencies.now = time.Now
	}
	executable, err := dependencies.resolveExecutable()
	if err != nil {
		return marker, ErrCodexValidationClientUnavailable
	}
	runVersion := func(ctx context.Context, path string, proof codexInstalledExecutableProof) ([]byte, error) {
		output, err := dependencies.runVersion(ctx, path, proof)
		if err != nil {
			return nil, ErrCodexValidationClientUnavailable
		}
		observed, ok := parseCodexInstalledVersionOutput(output)
		if !ok {
			return nil, ErrCodexValidationClientUnavailable
		}
		if observed != clientBuild {
			return nil, ErrCodexValidationBuildMismatch
		}
		return output, nil
	}
	var versionErr error
	probe, err := newCodexInstalledClientExecutableBuildProbe(ctx, executable, clientBuild, dependencies.captureExecutable, func(ctx context.Context, path string, proof codexInstalledExecutableProof) ([]byte, error) {
		output, err := runVersion(ctx, path, proof)
		versionErr = err
		return output, err
	})
	if err != nil {
		if versionErr != nil {
			return marker, versionErr
		}
		return marker, ErrCodexValidationClientUnavailable
	}
	evidence, err := runCodexInstalledWebSocketAcceptanceWithCleanup(ctx, dependencies.cleanupContext, cqBuild, clientBuild, probe.baseline, dependencies.runner)
	if err != nil {
		return marker, codexInstalledWebSocketValidationStageError("isolated client")
	}
	if _, err := probe.Probe(ctx); err != nil {
		return marker, errors.Join(ErrCodexValidationClientUnavailable, err)
	}
	if err := ctx.Err(); err != nil {
		return marker, err
	}
	if dependencies.cleanupContext != nil {
		if err := dependencies.cleanupContext.Err(); err != nil {
			return marker, err
		}
	}
	_, required := DefaultCodexRoutingRequirements(cqBuild, clientBuild)
	marker, err = buildCodexWebSocketReadinessMarker(evidence, required, dependencies.now().UTC())
	if err != nil {
		return CodexReadinessMarker{}, err
	}
	if err := saveCodexWebSocketReadinessMarkerDurably(markerDir, marker); err != nil {
		return CodexReadinessMarker{}, err
	}
	return marker, nil
}

func codexInstalledWebSocketValidationStageError(stage string) error {
	return fmt.Errorf("%w: %s", errCodexInstalledListenerAcceptance, stage)
}

type codexInstalledWebSocketTraffic struct {
	downstreamConnections atomic.Uint64
	webSocketRequests     atomic.Uint64
	upstreamDials         atomic.Uint64
	unexpectedRoutes      atomic.Uint64
	completedPrewarm      atomic.Bool
}

func runCodexInstalledWebSocketAcceptance(
	ctx context.Context,
	cqBuild string,
	clientBuild string,
	executable codexInstalledExecutableProof,
	runner codexAcceptanceRunner,
) (evidence CodexWebSocketReadinessEvidence, returnErr error) {
	return runCodexInstalledWebSocketAcceptanceWithCleanup(ctx, nil, cqBuild, clientBuild, executable, runner)
}
func runCodexInstalledWebSocketAcceptanceWithCleanup(ctx, cleanup context.Context, cqBuild, clientBuild string, executable codexInstalledExecutableProof, runner codexAcceptanceRunner) (evidence CodexWebSocketReadinessEvidence, returnErr error) {
	if ctx == nil || ctx.Err() != nil || !executable.valid() || runner == nil {
		return evidence, errCodexInstalledListenerAcceptance
	}
	core, err := newCodexInstalledHTTPValidationRuntimeCoreWithCleanup(ctx, cleanup)
	if err != nil {
		return evidence, err
	}
	trafficCtx, cancelTraffic := context.WithCancel(ctx)
	var requests sync.WaitGroup
	var activeRequests atomic.Int64
	var servers []*http.Server
	var closeOnce sync.Once
	var closeErr error
	closeTraffic := func() error {
		closeOnce.Do(func() {
			cancelTraffic()
			if cleanup == nil {
				// Legacy callers retain their bounded per-resource cleanup and runner.
				for _, server := range servers {
					shutdownCodexAcceptanceServer(server)
				}
				closeErr = core.close()
				return
			}
			for _, server := range servers {
				closeErr = errors.Join(closeErr, shutdownCodexAcceptanceServerContext(cleanup, server))
			}
			if activeRequests.Load() == 0 {
				closeErr = errors.Join(closeErr, core.closeWithContext(cleanup))
				return
			}
			drained := make(chan struct{})
			go func() { defer close(drained); requests.Wait() }()
			select {
			case <-drained:
				closeErr = errors.Join(closeErr, core.closeWithContext(cleanup))
			case <-cleanup.Done():
				closeErr = errors.Join(closeErr, cleanup.Err())
			}
		})
		return closeErr
	}
	defer func() { returnErr = errors.Join(returnErr, closeTraffic()) }()
	track := func(handler http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			activeRequests.Add(1)
			defer requests.Done()
			defer activeRequests.Add(-1)
			requestCtx, cancel := context.WithCancel(r.Context())
			stop := context.AfterFunc(trafficCtx, cancel)
			defer stop()
			defer cancel()
			handler.ServeHTTP(w, r.WithContext(requestCtx))
		})
	}
	localToken, err := newCodexInstalledHTTPValidationToken()
	if err != nil {
		return evidence, err
	}
	traffic := &codexInstalledWebSocketTraffic{}
	upstreamListener, upstreamServer, upstreamErrors, err := startCodexAcceptanceHTTP(track(http.HandlerFunc(traffic.serveUpstream)))
	if err != nil {
		return evidence, errCodexInstalledListenerAcceptance
	}
	servers = append(servers, upstreamServer)
	upstreamURL := "http://" + upstreamListener.Addr().String()
	planner := &CodexHTTPRequestPlanFactory{
		Inventory:         core.inventory,
		Capacity:          core.capacity,
		Routes:            core.continuity,
		Runtime:           core.leaseRuntime,
		DefaultAccountKey: codexInstalledHTTPValidationDefault,
		Authority: CodexLeaseAuthorityPolicy{
			ModeEpoch:     1,
			Authoritative: true,
		},
		Now: time.Now,
	}
	executor := NewCodexWebSocketAttemptExecutor(core.inventory, core.inventory)
	executor.Dialer.Proxy = nil
	broker, err := NewCodexTerminatingWebSocketHandler(planner, executor, core.inventory, core.capacity, upstreamURL)
	if err != nil {
		return evidence, errCodexInstalledListenerAcceptance
	}
	server := &Server{
		Config: &Config{
			LocalToken:     localToken,
			ClaudeUpstream: upstreamURL,
			CodexUpstream:  upstreamURL,
		},
		CodexRouting: &CodexRoutingRuntime{WebSocket: CodexModeStatus{
			Configured:         CodexRoutingEnforce,
			Effective:          CodexRoutingEnforce,
			ModeEpoch:          1,
			AuthoritativeEpoch: 1,
		}},
		CodexWebSocketBroker: broker,
		Catalog:              modelregistry.NewCatalog(modelregistry.Snapshot{}),
	}
	handler, err := server.handler()
	if err != nil {
		return evidence, errCodexInstalledListenerAcceptance
	}
	candidateHandler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == legacyCodexResponsesPath && websocket.IsWebSocketUpgrade(request) {
			traffic.downstreamConnections.Add(1)
		} else if !(request.Method == http.MethodGet && request.URL.Path == "/models") {
			traffic.unexpectedRoutes.Add(1)
		}
		handler.ServeHTTP(writer, request)
	})
	candidateListener, candidateServer, candidateErrors, err := startCodexAcceptanceHTTP(track(candidateHandler))
	if err != nil {
		return evidence, errCodexInstalledListenerAcceptance
	}
	servers = append(servers, candidateServer)
	outcome := &codexInstalledHTTPClientOutcome{}
	exercise, err := newCodexInstalledWebSocketClientExercise(candidateListener.Addr().String(), executable, localToken, runner, outcome)
	if err != nil {
		return evidence, errCodexInstalledListenerAcceptance
	}
	exercise.cleanupContext = cleanup
	if err := exercise.Run(ctx); err != nil {
		return evidence, err
	}
	if err := closeTraffic(); err != nil {
		return evidence, err
	}
	for _, serverErrors := range []<-chan error{candidateErrors, upstreamErrors} {
		if err := codexAcceptanceServeError(serverErrors); err != nil {
			return evidence, errCodexInstalledListenerAcceptance
		}
	}
	acceptance := CodexWebSocketAcceptanceResult{
		InstalledVersion:      clientBuild,
		DownstreamConnections: traffic.downstreamConnections.Load(),
		WebSocketRequests:     traffic.webSocketRequests.Load(),
		UpstreamDials:         traffic.upstreamDials.Load(),
		UnexpectedRoutes:      traffic.unexpectedRoutes.Load(),
		EgressAttempts:        outcome.egressAttempts.Load(),
		PongVerified:          outcome.exactPong.Load(),
	}
	if !traffic.completedPrewarm.Load() || acceptance.WebSocketRequests < 2 || acceptance.UpstreamDials != 1 {
		return evidence, errCodexInstalledListenerAcceptance
	}
	return CodexWebSocketReadinessEvidence{
		Source: CodexWebSocketReadinessEvidenceInstalledIsolated,
		Tuple:  readinessTupleForBuilds(cqBuild, clientBuild, CodexRoutingWebSocket),
		Gates: CodexWebSocketReadinessGateEvidence{
			StrongFrameAuthorityCases:             1,
			PortablePreAdmissionHard429Rotations:  1,
			SameAccountCandidateAuthRecoveryCases: 1,
			AdmittedNoMigrationCases:              1,
			PersistentAccountUpstreamCases:        1,
			UpstreamGenerationFenceCases:          1,
			CanonicalTerminalErrorCases:           1,
			CompressionSubprotocolCases:           1,
		},
		Acceptance: acceptance,
	}, nil
}

func (traffic *codexInstalledWebSocketTraffic) serveUpstream(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path != legacyCodexResponsesPath || !websocket.IsWebSocketUpgrade(request) {
		traffic.unexpectedRoutes.Add(1)
		http.NotFound(writer, request)
		return
	}
	if request.Header.Get("Authorization") == "" || request.Header.Get("ChatGPT-Account-ID") == "" {
		traffic.unexpectedRoutes.Add(1)
		http.Error(writer, "missing explicit authority", http.StatusUnauthorized)
		return
	}
	traffic.upstreamDials.Add(1)
	upgrader := websocket.Upgrader{
		CheckOrigin:       func(*http.Request) bool { return true },
		Subprotocols:      websocket.Subprotocols(request),
		EnableCompression: true,
	}
	connection, err := upgrader.Upgrade(writer, request, nil)
	if err != nil {
		return
	}
	defer connection.Close()
	stopClose := context.AfterFunc(request.Context(), func() { defer func() { _ = recover() }(); _ = connection.Close() })
	defer stopClose()
	for {
		messageType, frame, err := connection.ReadMessage()
		if err != nil {
			return
		}
		if messageType != websocket.TextMessage {
			traffic.unexpectedRoutes.Add(1)
			return
		}
		pending, err := newCodexWSPendingFrameOwned(messageType, frame)
		if err != nil {
			traffic.unexpectedRoutes.Add(1)
			return
		}
		prewarm := pending.prewarm
		previousResponseID := pending.request.PreviousResponseID
		pending.Release()
		traffic.webSocketRequests.Add(1)
		if prewarm {
			traffic.completedPrewarm.Store(true)
			for _, reply := range [][]byte{
				[]byte(`{"type":"response.created","response":{"id":"acceptance-prewarm"}}`),
				[]byte(`{"type":"response.completed","response":{"id":"acceptance-prewarm"}}`),
			} {
				if err := connection.WriteMessage(websocket.TextMessage, reply); err != nil {
					return
				}
			}
			continue
		}
		if traffic.completedPrewarm.Load() && previousResponseID != "acceptance-prewarm" {
			traffic.unexpectedRoutes.Add(1)
			_ = connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","status":400,"error":{"type":"invalid_request_error"}}`))
			return
		}
		for _, reply := range [][]byte{
			[]byte(`{"type":"response.created","response":{"id":"acceptance-response"}}`),
			[]byte(`{"type":"response.output_item.done","item":{"type":"message","role":"assistant","id":"acceptance-message","content":[{"type":"output_text","text":"PONG"}]}}`),
			[]byte(`{"type":"response.completed","response":{"id":"acceptance-response","end_turn":true,"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`),
		} {
			if err := connection.WriteMessage(websocket.TextMessage, reply); err != nil {
				return
			}
		}
	}
}

func readinessTupleForBuilds(cqBuild, clientBuild string, transport CodexRoutingTransport) CodexReadinessTuple {
	httpRequired, webSocketRequired := DefaultCodexRoutingRequirements(cqBuild, clientBuild)
	if transport == CodexRoutingWebSocket {
		return readinessTuple(webSocketRequired)
	}
	return readinessTuple(httpRequired)
}
