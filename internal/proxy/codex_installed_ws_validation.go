package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/modelregistry"
)

type codexInstalledWebSocketValidationDependencies struct {
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
	if strings.TrimSpace(markerDir) == "" {
		paths, err := ResolveDefaultPaths()
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
		resolveExecutable: resolveExecutable,
		captureExecutable: captureCodexInstalledExecutable,
		runVersion:        runCodexInstalledVersionCommand,
		runner:            osCodexAcceptanceRunner{},
		now:               time.Now,
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
		if returnErr != nil {
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
		return marker, codexInstalledWebSocketValidationStageError("client executable")
	}
	probe, err := newCodexInstalledClientExecutableBuildProbe(ctx, executable, clientBuild, dependencies.captureExecutable, dependencies.runVersion)
	if err != nil {
		return marker, codexInstalledWebSocketValidationStageError("client build")
	}
	evidence, err := runCodexInstalledWebSocketAcceptance(ctx, cqBuild, clientBuild, probe.baseline, dependencies.runner)
	if err != nil {
		return marker, codexInstalledWebSocketValidationStageError("isolated client")
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
	if ctx == nil || ctx.Err() != nil || !executable.valid() || runner == nil {
		return evidence, errCodexInstalledListenerAcceptance
	}
	core, err := newCodexInstalledHTTPValidationRuntimeCore(ctx)
	if err != nil {
		return evidence, err
	}
	defer func() { returnErr = errors.Join(returnErr, core.close()) }()
	localToken, err := newCodexInstalledHTTPValidationToken()
	if err != nil {
		return evidence, err
	}
	traffic := &codexInstalledWebSocketTraffic{}
	upstreamListener, upstreamServer, upstreamErrors, err := startCodexAcceptanceHTTP(http.HandlerFunc(traffic.serveUpstream))
	if err != nil {
		return evidence, errCodexInstalledListenerAcceptance
	}
	defer shutdownCodexAcceptanceServer(upstreamServer)
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
	candidateListener, candidateServer, candidateErrors, err := startCodexAcceptanceHTTP(candidateHandler)
	if err != nil {
		return evidence, errCodexInstalledListenerAcceptance
	}
	defer shutdownCodexAcceptanceServer(candidateServer)
	outcome := &codexInstalledHTTPClientOutcome{}
	exercise, err := newCodexInstalledWebSocketClientExercise(candidateListener.Addr().String(), executable, localToken, runner, outcome)
	if err != nil {
		return evidence, errCodexInstalledListenerAcceptance
	}
	if err := exercise.Run(ctx); err != nil {
		return evidence, err
	}
	shutdownCodexAcceptanceServer(candidateServer)
	shutdownCodexAcceptanceServer(upstreamServer)
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
	for {
		messageType, frame, err := connection.ReadMessage()
		if err != nil {
			return
		}
		if messageType != websocket.TextMessage {
			traffic.unexpectedRoutes.Add(1)
			return
		}
		pending, err := newCodexWSPendingFrame(messageType, frame)
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
