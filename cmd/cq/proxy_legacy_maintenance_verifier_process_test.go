//go:build unix

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func TestLegacyMaintenanceAcquireReturnsProcessLeaseAfterHeadroomAndSignedHealth(t *testing.T) {
	t.Parallel()
	health, runtime := healthyLegacyMaintenanceRuntimeFixture()
	proofServer := startLegacyMaintenanceProofServer(t, health)
	executable := legacyMaintenanceExecutableProof{
		path: "/private/test/candidate", device: 1, inode: 2, links: 1, size: 3, mode: 0o755,
		sha256: sha256.Sum256([]byte("candidate executable")),
	}
	var probes atomic.Int32
	verifier := &proxyLegacyMaintenanceFinaliseVerifier{
		build:       "candidate-build",
		clientBuild: "client-build",
		healthURL:   "http://" + proofServer.listener.Addr().String() + "/health",
		healthAddr:  proofServer.listener.Addr().String(),
		attestor:    proofServer.attestor,
		dialContext: (&net.Dialer{}).DialContext,
		executable:  executable,
		capture:     func() (legacyMaintenanceExecutableProof, error) { return executable, nil },
		runtime:     runtime,
		frozen:      cloneLegacyMaintenanceRoutingRuntime(*runtime),
		headroomProbe: legacyMaintenanceHeadroomProberFunc(func(ctx context.Context) error {
			if _, ok := ctx.Deadline(); !ok {
				t.Error("headroom probe context has no deadline")
			}
			probes.Add(1)
			return nil
		}),
		headroom:     true,
		headroomMode: proxy.HeadroomModeCache,
		bound:        true,
	}
	var dials atomic.Int32
	verifier.dialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp4" || address != proofServer.listener.Addr().String() {
			t.Errorf("dial = %s %s", network, address)
		}
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	proof := codexprov.LegacyMaintenanceFinaliseVerification{
		TicketHash: stringsRepeat("a", 64), OwnerGeneration: stringsRepeat("b", 32),
	}

	lease, err := verifier.AcquireLegacyMaintenanceFinalise(context.Background(), proof)
	if err != nil {
		t.Fatalf("Acquire error = %v", err)
	}
	if lease == nil {
		t.Fatal("Acquire returned a nil process lease")
	}
	defer lease.Release()
	servingLease, ok := lease.(proxy.ServingProofLease)
	if !ok {
		t.Fatalf("Acquire lease type = %T, want serving proof lease", lease)
	}
	if err := servingLease.Seal(); !errors.Is(err, proxy.ErrServingProofInvalid) {
		t.Fatalf("second Seal error = %v, want already-sealed proof rejection", err)
	}
	if got := probes.Load(); got != 1 {
		t.Fatalf("headroom probes = %d, want 1", got)
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("health dials = %d, want one fresh connection", got)
	}
	if err := proofServer.listener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-proofServer.serveDone:
		t.Fatalf("fatal listener failure crossed the verifier-acquired lease: %v", proofServer.serveError())
	default:
	}
	lease.Release()
	select {
	case <-proofServer.serveDone:
		if err := proofServer.serveError(); err == nil {
			t.Fatal("fatal listener failure was reported as success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fatal listener failure did not return after process lease release")
	}
}

func TestLegacyMaintenanceAcquireFailsClosedBeforeHealthWhenHeadroomProbeFails(t *testing.T) {
	t.Parallel()
	health, runtime := healthyLegacyMaintenanceRuntimeFixture()
	proofServer := startLegacyMaintenanceProofServer(t, health)
	executable := legacyMaintenanceExecutableProof{
		path: "/private/test/candidate", device: 1, inode: 2, links: 1, size: 3, mode: 0o755,
		sha256: sha256.Sum256([]byte("candidate executable")),
	}
	var probes atomic.Int32
	verifier := &proxyLegacyMaintenanceFinaliseVerifier{
		build:       "candidate-build",
		clientBuild: "client-build",
		healthURL:   "http://" + proofServer.listener.Addr().String() + "/health",
		healthAddr:  proofServer.listener.Addr().String(),
		attestor:    proofServer.attestor,
		dialContext: (&net.Dialer{}).DialContext,
		executable:  executable,
		capture:     func() (legacyMaintenanceExecutableProof, error) { return executable, nil },
		runtime:     runtime,
		frozen:      cloneLegacyMaintenanceRoutingRuntime(*runtime),
		headroomProbe: legacyMaintenanceHeadroomProberFunc(func(context.Context) error {
			probes.Add(1)
			return errors.New("PRIVATE_HEADROOM_FAILURE")
		}),
		headroom:     true,
		headroomMode: proxy.HeadroomModeCache,
		bound:        true,
	}
	proof := codexprov.LegacyMaintenanceFinaliseVerification{
		TicketHash: stringsRepeat("a", 64), OwnerGeneration: stringsRepeat("b", 32),
	}
	lease, err := verifier.AcquireLegacyMaintenanceFinalise(context.Background(), proof)
	if lease != nil || !errors.Is(err, errLegacyMaintenanceRuntimeNotReady) {
		t.Fatalf("Acquire = %v, %v", lease, err)
	}
	if bytes.Contains([]byte(err.Error()), []byte("PRIVATE_HEADROOM_FAILURE")) {
		t.Fatalf("Acquire leaked private probe failure: %q", err)
	}
	if got := probes.Load(); got != 1 {
		t.Fatalf("headroom probes = %d, want 1", got)
	}
	if got := proofServer.requests.Load(); got != 0 {
		t.Fatalf("health requests after failed probe = %d", got)
	}
}

func TestLegacyMaintenanceAcquireRejectsNilContext(t *testing.T) {
	t.Parallel()
	health, runtime := healthyLegacyMaintenanceRuntimeFixture()
	proofServer := startLegacyMaintenanceProofServer(t, health)
	verifier, proof := newLegacyMaintenanceProcessVerifier(t, proofServer, runtime)
	lease, err := verifier.AcquireLegacyMaintenanceFinalise(nil, proof)
	if lease != nil || !errors.Is(err, errLegacyMaintenanceRuntimeNotReady) {
		t.Fatalf("nil-context Acquire = %v, %v", lease, err)
	}
	if got := proofServer.requests.Load(); got != 0 {
		t.Fatalf("health requests with nil context = %d, want 0", got)
	}
}

func TestLegacyMaintenanceAcquireUsesFreshHealthAndFrozenRuntime(t *testing.T) {
	t.Parallel()
	health, runtime := healthyLegacyMaintenanceRuntimeFixture()
	var healthMu sync.RWMutex
	proofServer := startLegacyMaintenanceProofServerFunc(t, func() legacyMaintenanceRuntimeHealth {
		healthMu.RLock()
		defer healthMu.RUnlock()
		return health
	})
	verifier, proof := newLegacyMaintenanceProcessVerifier(t, proofServer, runtime)

	lease, err := verifier.AcquireLegacyMaintenanceFinalise(context.Background(), proof)
	if err != nil {
		t.Fatalf("first Acquire error = %v", err)
	}
	lease.Release()
	healthMu.Lock()
	health.RoutingDefault.Resolved = false
	healthMu.Unlock()
	if lease, err := verifier.AcquireLegacyMaintenanceFinalise(context.Background(), proof); lease != nil || !errors.Is(err, errLegacyMaintenanceRuntimeNotReady) {
		t.Fatalf("unhealthy Acquire = %v, %v", lease, err)
	}
	if got := proofServer.requests.Load(); got != 2 {
		t.Fatalf("fresh health requests = %d, want 2", got)
	}

	healthMu.Lock()
	health.RoutingDefault.Resolved = true
	healthMu.Unlock()
	runtime.HTTP.InhibitionReason = "toggled"
	if lease, err := verifier.AcquireLegacyMaintenanceFinalise(context.Background(), proof); lease != nil || !errors.Is(err, errLegacyMaintenanceRuntimeNotReady) {
		t.Fatalf("mutated runtime Acquire = %v, %v", lease, err)
	}
	if got := proofServer.requests.Load(); got != 2 {
		t.Fatalf("health requests after frozen runtime mismatch = %d, want 2", got)
	}
}

func TestLegacyMaintenanceAcquireRejectsExecutableReplacementAndHidesAuthority(t *testing.T) {
	t.Parallel()
	health, runtime := healthyLegacyMaintenanceRuntimeFixture()
	proofServer := startLegacyMaintenanceProofServer(t, health)
	verifier, proof := newLegacyMaintenanceProcessVerifier(t, proofServer, runtime)
	replacement := verifier.executable
	replacement.inode++
	verifier.capture = func() (legacyMaintenanceExecutableProof, error) { return replacement, nil }

	lease, err := verifier.AcquireLegacyMaintenanceFinalise(context.Background(), proof)
	if lease != nil || !errors.Is(err, errLegacyMaintenanceRuntimeNotReady) {
		t.Fatalf("replacement Acquire = %v, %v", lease, err)
	}
	message := err.Error()
	if strings.Contains(message, proof.TicketHash) || strings.Contains(message, proof.OwnerGeneration) || strings.Contains(message, verifier.executable.path) {
		t.Fatalf("verification error leaked authority or executable path: %q", message)
	}
	if got := proofServer.requests.Load(); got != 0 {
		t.Fatalf("health requests after executable replacement = %d, want 0", got)
	}
}

func TestLegacyMaintenanceAcquireRejectsUnboundServingAuthorityBeforeHealth(t *testing.T) {
	t.Parallel()
	health, runtime := healthyLegacyMaintenanceRuntimeFixture()
	proofServer := startLegacyMaintenanceProofServer(t, health)
	verifier, proof := newLegacyMaintenanceProcessVerifier(t, proofServer, runtime)
	verifier.attestor = proxy.NewServingAttestor()

	lease, err := verifier.AcquireLegacyMaintenanceFinalise(context.Background(), proof)
	if lease != nil || !errors.Is(err, errLegacyMaintenanceProcessProofUnavailable) {
		t.Fatalf("unbound attestor Acquire = %v, %v", lease, err)
	}
	if got := proofServer.requests.Load(); got != 0 {
		t.Fatalf("foreign health requests = %d, want 0 before process proof", got)
	}
}

func TestLegacyMaintenanceAcquireRejectsAlteredProofTranscriptAndReleasesLease(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate legacyMaintenanceProofResponseMutator
	}{
		{name: "body after signing", mutate: func(body []byte, proof string) (int, []byte, []string, string) {
			return http.StatusOK, append(append([]byte(nil), body...), ' '), []string{proof}, ""
		}},
		{name: "duplicate proof", mutate: func(body []byte, proof string) (int, []byte, []string, string) {
			return http.StatusOK, body, []string{proof, proof}, ""
		}},
		{name: "malformed proof", mutate: func(body []byte, _ string) (int, []byte, []string, string) {
			return http.StatusOK, body, []string{"not-a-proof"}, ""
		}},
		{name: "encoded response", mutate: func(body []byte, proof string) (int, []byte, []string, string) {
			return http.StatusOK, body, []string{proof}, "gzip"
		}},
		{name: "redirect", mutate: func(body []byte, proof string) (int, []byte, []string, string) {
			return http.StatusTemporaryRedirect, body, []string{proof}, ""
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			health, runtime := healthyLegacyMaintenanceRuntimeFixture()
			proofServer := startLegacyMaintenanceProofServerWithMutator(t, func() legacyMaintenanceRuntimeHealth { return health }, test.mutate)
			verifier, proof := newLegacyMaintenanceProcessVerifier(t, proofServer, runtime)

			lease, err := verifier.AcquireLegacyMaintenanceFinalise(context.Background(), proof)
			if lease != nil || !errors.Is(err, errLegacyMaintenanceRuntimeNotReady) {
				t.Fatalf("altered transcript Acquire = %v, %v", lease, err)
			}
			select {
			case <-proofServer.attestor.BeginClose():
			case <-time.After(2 * time.Second):
				t.Fatal("failed transcript leaked its serving lease")
			}
			if got := proofServer.requests.Load(); got != 1 {
				t.Fatalf("altered transcript health requests = %d, want 1", got)
			}
		})
	}
}

func TestLegacyMaintenanceProcessBindingVector(t *testing.T) {
	t.Parallel()
	_, runtime := healthyLegacyMaintenanceRuntimeFixture()
	executable := legacyMaintenanceExecutableProof{
		path: "/private/test/candidate", device: 0x01020304, inode: 0x05060708, links: 1,
		size: 0x11121314, mode: 0o755,
		sha256: sha256.Sum256([]byte("candidate executable vector")),
	}
	proof := codexprov.LegacyMaintenanceFinaliseVerification{
		TicketHash: stringsRepeat("ab", 32), OwnerGeneration: stringsRepeat("cd", 16),
	}
	binding, err := legacyMaintenanceProcessBinding(proof, "candidate-build", "client-build", executable, *runtime)
	if err != nil {
		t.Fatal(err)
	}
	const want = "3b1521e4933da308b6a74889f51951fde4fc15e97606ebe0f16358dc161f9031"
	if got := hex.EncodeToString(binding[:]); got != want {
		t.Fatalf("binding vector = %s, want %s", got, want)
	}
	assertDifferent := func(name string, candidateProof codexprov.LegacyMaintenanceFinaliseVerification, build, clientBuild string, candidateExecutable legacyMaintenanceExecutableProof, candidateRuntime proxy.CodexRoutingRuntime) {
		t.Helper()
		candidate, err := legacyMaintenanceProcessBinding(candidateProof, build, clientBuild, candidateExecutable, candidateRuntime)
		if err != nil {
			t.Fatalf("%s binding error = %v", name, err)
		}
		if candidate == binding {
			t.Fatalf("%s did not change the process binding", name)
		}
	}
	changedProof := proof
	changedProof.TicketHash = stringsRepeat("ac", 32)
	assertDifferent("ticket", changedProof, "candidate-build", "client-build", executable, *runtime)
	changedProof = proof
	changedProof.OwnerGeneration = stringsRepeat("ce", 16)
	assertDifferent("owner generation", changedProof, "candidate-build", "client-build", executable, *runtime)
	assertDifferent("build", proof, "other-build", "client-build", executable, *runtime)
	assertDifferent("client build", proof, "candidate-build", "other-client", executable, *runtime)
	changedExecutable := executable
	changedExecutable.inode++
	assertDifferent("executable", proof, "candidate-build", "client-build", changedExecutable, *runtime)
	changedRuntime := cloneLegacyMaintenanceRoutingRuntime(*runtime)
	changedRuntime.HTTP.ModeEpoch++
	assertDifferent("runtime", proof, "candidate-build", "client-build", executable, changedRuntime)
}

type legacyMaintenanceProofServer struct {
	listener  *net.TCPListener
	attestor  *proxy.ServingAttestor
	requests  atomic.Int32
	server    *http.Server
	health    func() legacyMaintenanceRuntimeHealth
	serveDone chan struct{}
	serveMu   sync.Mutex
	serveErr  error
}

func (s *legacyMaintenanceProofServer) serveError() error {
	s.serveMu.Lock()
	defer s.serveMu.Unlock()
	return s.serveErr
}

type legacyMaintenanceProofResponseMutator func(body []byte, proof string) (status int, responseBody []byte, proofs []string, contentEncoding string)

func startLegacyMaintenanceProofServer(t *testing.T, health legacyMaintenanceRuntimeHealth) *legacyMaintenanceProofServer {
	t.Helper()
	return startLegacyMaintenanceProofServerFunc(t, func() legacyMaintenanceRuntimeHealth { return health })
}

func startLegacyMaintenanceProofServerFunc(t *testing.T, health func() legacyMaintenanceRuntimeHealth) *legacyMaintenanceProofServer {
	t.Helper()
	return startLegacyMaintenanceProofServerWithMutator(t, health, nil)
}

func startLegacyMaintenanceProofServerWithMutator(t *testing.T, health func() legacyMaintenanceRuntimeHealth, mutate legacyMaintenanceProofResponseMutator) *legacyMaintenanceProofServer {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	attestor := proxy.NewServingAttestor()
	serveListener, err := attestor.ActivateListener(listener)
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	fixture := &legacyMaintenanceProofServer{
		listener: listener, attestor: attestor, health: health, serveDone: make(chan struct{}),
	}
	fixture.server = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		fixture.requests.Add(1)
		body, err := json.Marshal(fixture.health())
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		status := http.StatusOK
		responseBody := body
		var proofs []string
		if signed, ok := attestor.ProveHealth(request, body); ok {
			proofs = []string{signed}
		}
		contentEncoding := ""
		if mutate != nil {
			status, responseBody, proofs, contentEncoding = mutate(body, firstLegacyMaintenanceProof(proofs))
		}
		for _, proof := range proofs {
			w.Header().Add(proxy.ServingProofResponseHeader, proof)
		}
		if contentEncoding != "" {
			w.Header().Set("Content-Encoding", contentEncoding)
		}
		if status == http.StatusTemporaryRedirect {
			w.Header().Set("Location", "/health")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(responseBody)
	})}
	go func() {
		err := fixture.server.Serve(serveListener)
		fixture.serveMu.Lock()
		fixture.serveErr = err
		fixture.serveMu.Unlock()
		close(fixture.serveDone)
	}()
	t.Cleanup(func() {
		_ = fixture.server.Close()
		select {
		case <-fixture.serveDone:
		case <-time.After(2 * time.Second):
			t.Error("proof server did not stop")
		}
		select {
		case <-attestor.BeginClose():
		case <-time.After(2 * time.Second):
			t.Error("proof server attestor did not close")
		}
	})
	return fixture
}

func firstLegacyMaintenanceProof(proofs []string) string {
	if len(proofs) == 0 {
		return ""
	}
	return proofs[0]
}

func newLegacyMaintenanceProcessVerifier(t *testing.T, proofServer *legacyMaintenanceProofServer, runtime *proxy.CodexRoutingRuntime) (*proxyLegacyMaintenanceFinaliseVerifier, codexprov.LegacyMaintenanceFinaliseVerification) {
	t.Helper()
	executable := legacyMaintenanceExecutableProof{
		path: "/private/test/candidate", device: 1, inode: 2, links: 1, size: 3, mode: 0o755,
		sha256: sha256.Sum256([]byte("candidate executable")),
	}
	verifier := &proxyLegacyMaintenanceFinaliseVerifier{
		build:       "candidate-build",
		clientBuild: "client-build",
		healthURL:   "http://" + proofServer.listener.Addr().String() + "/health",
		healthAddr:  proofServer.listener.Addr().String(),
		attestor:    proofServer.attestor,
		dialContext: (&net.Dialer{}).DialContext,
		executable:  executable,
		capture:     func() (legacyMaintenanceExecutableProof, error) { return executable, nil },
		runtime:     runtime,
		frozen:      cloneLegacyMaintenanceRoutingRuntime(*runtime),
		headroomProbe: legacyMaintenanceHeadroomProberFunc(func(ctx context.Context) error {
			if _, ok := ctx.Deadline(); !ok {
				t.Error("headroom probe context has no deadline")
			}
			return nil
		}),
		headroom:     true,
		headroomMode: proxy.HeadroomModeCache,
		bound:        true,
	}
	return verifier, codexprov.LegacyMaintenanceFinaliseVerification{
		TicketHash: stringsRepeat("a", 64), OwnerGeneration: stringsRepeat("b", 32),
	}
}

func healthyLegacyMaintenanceRuntimeFixture() (legacyMaintenanceRuntimeHealth, *proxy.CodexRoutingRuntime) {
	runtime := &proxy.CodexRoutingRuntime{
		HTTP: proxy.CodexModeStatus{
			Configured: proxy.CodexRoutingEnforce, Effective: proxy.CodexRoutingEnforce,
			ModeEpoch: 1, AuthoritativeEpoch: 1,
		},
		WebSocket: proxy.CodexModeStatus{
			Configured: proxy.CodexRoutingObserve, Effective: proxy.CodexRoutingObserve,
			ModeEpoch: 2, ShadowEpoch: 2,
		},
	}
	accounts := 3
	return legacyMaintenanceRuntimeHealth{
		Status: "ok", Headroom: true, HeadroomMode: "cache",
		Accounts: legacyMaintenanceAccountHealth{Codex: &accounts}, InventoryHealth: "ok",
		ExternalSources: []proxy.CodexSourceHealth{{Name: "external", CandidateCount: 1, HealthCode: "ok"}},
		HTTP:            runtime.HTTP, WebSocket: runtime.WebSocket,
		RoutingDefault: legacyMaintenanceDefaultHealth{Configured: true, Resolved: true, Routable: true, Status: "resolved"},
	}, runtime
}

func stringsRepeat(value string, count int) string {
	var result bytes.Buffer
	for range count {
		result.WriteString(value)
	}
	return result.String()
}
