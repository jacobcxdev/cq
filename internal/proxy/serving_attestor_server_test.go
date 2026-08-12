package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"testing"
	"time"
)

func TestServerServingProofUsesExactListenerConnectionAndBody(t *testing.T) {
	t.Parallel()
	listener := listenServingAttestorTestTCP4(t)
	attestor := NewServingAttestor()
	server := &Server{
		Config: &Config{
			ClaudeUpstream: "https://api.anthropic.com",
			LocalToken:     "test-token",
		},
		ServingAttestor: attestor,
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.serve(ctx, listener) }()

	lease := acquireServingProofTestLease(t, attestor)
	body, proof, local, remote := requestServingProofTestHealth(t, listener, lease.Challenge())
	if err := lease.VerifyResponse(body, proof, local, remote); err != nil {
		t.Fatalf("VerifyResponse error = %v", err)
	}
	lease.Release()

	ordinaryBody, ordinaryProof, _, _ := requestServingProofTestHealth(t, listener, "")
	if ordinaryProof != "" {
		t.Fatal("ordinary health response exposed a serving proof")
	}
	provedStable := servingProofTestStableHealthBody(t, body)
	ordinaryStable := servingProofTestStableHealthBody(t, ordinaryBody)
	if !bytes.Equal(ordinaryStable, provedStable) {
		t.Fatalf("ordinary health body changed under attestation\nordinary: %s\nproved: %s", ordinaryBody, body)
	}

	cancel()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("serve error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not stop")
	}
}

func servingProofTestStableHealthBody(t *testing.T, body []byte) []byte {
	t.Helper()
	var health map[string]json.RawMessage
	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatalf("decode health body: %v", err)
	}
	runtime, present := health["codex_runtime_observability"]
	if !present {
		t.Fatal("health body omitted codex_runtime_observability")
	}
	var aggregate codexRuntimeObservabilitySnapshot
	if err := json.Unmarshal(runtime, &aggregate); err != nil {
		t.Fatalf("decode aggregate runtime observability: %v", err)
	}
	delete(health, "codex_runtime_observability")
	stable, err := json.Marshal(health)
	if err != nil {
		t.Fatalf("encode stable health body: %v", err)
	}
	return stable
}

func TestServerShutdownRejectsNewProofsAndWaitsForAcquiredLease(t *testing.T) {
	t.Parallel()
	listener := listenServingAttestorTestTCP4(t)
	attestor := NewServingAttestor()
	server := &Server{
		Config:          &Config{ClaudeUpstream: "https://api.anthropic.com", LocalToken: "test-token"},
		ServingAttestor: attestor,
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.serve(ctx, listener) }()
	lease := acquireServingProofTestLease(t, attestor)

	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for {
		late, err := attestor.Acquire(sha256.Sum256([]byte("late binding")))
		if errors.Is(err, ErrServingAttestorClosing) {
			break
		}
		if err == nil {
			late.Release()
			time.Sleep(time.Millisecond)
			continue
		}
		if time.Now().After(deadline) {
			t.Fatalf("attestor did not begin closing: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-serveDone:
		t.Fatalf("server stopped before acquired lease released: %v", err)
	default:
	}
	body, proof, local, remote := requestServingProofTestHealth(t, listener, lease.Challenge())
	if err := lease.VerifyResponse(body, proof, local, remote); err != nil {
		t.Fatalf("pre-issued proof failed after shutdown linearised: %v", err)
	}
	lease.Release()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("serve error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not stop after lease release")
	}
}

func TestServerUnexpectedListenerFailureRevokesUnsealedProofLease(t *testing.T) {
	t.Parallel()
	listener := listenServingAttestorTestTCP4(t)
	attestor := NewServingAttestor()
	server := &Server{
		Config:          &Config{ClaudeUpstream: "https://api.anthropic.com", LocalToken: "test-token"},
		ServingAttestor: attestor,
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.serve(context.Background(), listener) }()
	lease := acquireServingProofTestLease(t, attestor)
	body, proof, local, remote := requestServingProofTestHealth(t, listener, lease.Challenge())
	if err := lease.VerifyResponse(body, proof, local, remote); err != nil {
		t.Fatalf("VerifyResponse error = %v", err)
	}

	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-serveDone:
		if err == nil {
			t.Fatal("unexpected listener failure was reported as success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("unexpected listener failure waited for an unsealed proof lease")
	}
	if err := lease.Seal(); !errors.Is(err, ErrServingProofInvalid) {
		t.Fatalf("Seal after listener failure error = %v, want invalid proof", err)
	}
	lease.Release()
}

func TestServerUnexpectedListenerFailureWaitsForSealedProofLease(t *testing.T) {
	t.Parallel()
	listener := listenServingAttestorTestTCP4(t)
	attestor := NewServingAttestor()
	server := &Server{
		Config:          &Config{ClaudeUpstream: "https://api.anthropic.com", LocalToken: "test-token"},
		ServingAttestor: attestor,
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.serve(context.Background(), listener) }()
	lease := acquireServingProofTestLease(t, attestor)
	body, proof, local, remote := requestServingProofTestHealth(t, listener, lease.Challenge())
	if err := lease.VerifyResponse(body, proof, local, remote); err != nil {
		t.Fatalf("VerifyResponse error = %v", err)
	}
	if err := lease.Seal(); err != nil {
		t.Fatalf("Seal error = %v", err)
	}

	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-serveDone:
		t.Fatalf("unexpected listener failure crossed a sealed operation: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if late, err := attestor.Acquire(sha256.Sum256([]byte("late binding"))); late != nil || !errors.Is(err, ErrServingAttestorClosing) {
		t.Fatalf("Acquire after listener failure = %v, %v", late, err)
	}
	lease.Release()
	select {
	case err := <-serveDone:
		if err == nil {
			t.Fatal("unexpected listener failure was reported as success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("unexpected listener failure did not return after sealed lease release")
	}
}

func TestServerFatalListenerFailureEscalatesGracefulCloseAndRevokesUnsealedLease(t *testing.T) {
	t.Parallel()
	listener := listenServingAttestorTestTCP4(t)
	attestor := NewServingAttestor()
	server := &Server{
		Config:          &Config{ClaudeUpstream: "https://api.anthropic.com", LocalToken: "test-token"},
		ServingAttestor: attestor,
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.serve(context.Background(), listener) }()
	lease := acquireServingProofTestLease(t, attestor)
	body, proof, local, remote := requestServingProofTestHealth(t, listener, lease.Challenge())
	if err := lease.VerifyResponse(body, proof, local, remote); err != nil {
		t.Fatalf("VerifyResponse error = %v", err)
	}
	closeDone := attestor.BeginClose()
	select {
	case <-closeDone:
		t.Fatal("graceful close completed while an unsealed lease was held")
	default:
	}

	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("fatal listener error did not unblock the earlier graceful waiter")
	}
	if err := lease.Seal(); !errors.Is(err, ErrServingProofInvalid) {
		t.Fatalf("Seal after escalated listener failure error = %v, want invalid proof", err)
	}
	select {
	case err := <-serveDone:
		if err == nil {
			t.Fatal("fatal listener failure was reported as success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fatal listener failure did not return after graceful escalation")
	}
	lease.Release()
}

func TestServingAttestedListenerDoesNotAbortOnTemporaryAcceptError(t *testing.T) {
	t.Parallel()
	listener := listenServingAttestorTestTCP4(t)
	attestor := NewServingAttestor()
	if err := attestor.activate(listener); err != nil {
		t.Fatal(err)
	}
	guarded := &servingAttestedTCP4Listener{
		Listener: temporaryErrorListener{address: listener.Addr()},
		attestor: attestor,
	}
	if _, err := guarded.Accept(); err == nil {
		t.Fatal("temporary Accept unexpectedly succeeded")
	}
	lease, err := attestor.Acquire(sha256.Sum256([]byte("binding after temporary error")))
	if err != nil {
		t.Fatalf("temporary Accept error revoked authority: %v", err)
	}
	lease.Release()
	select {
	case <-attestor.BeginClose():
	case <-time.After(2 * time.Second):
		t.Fatal("attestor did not close after temporary Accept test")
	}
}

func TestServerShutdownTimeoutIsReportedAndForcesConnectionClosed(t *testing.T) {
	t.Parallel()
	listener := listenServingAttestorTestTCP4(t)
	handlerEntered := make(chan struct{})
	allowHandler := make(chan struct{})
	server := &Server{
		Config: &Config{ClaudeUpstream: "https://api.anthropic.com", LocalToken: "test-token"},
		CodexHealth: func() CodexHealth {
			select {
			case <-handlerEntered:
			default:
				close(handlerEntered)
			}
			<-allowHandler
			return CodexHealth{AccountCountKnown: true}
		},
		shutdownGracePeriod: 20 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.serve(ctx, listener) }()

	requestDone := make(chan error, 1)
	go func() {
		response, err := http.Get("http://" + listener.Addr().String() + "/health")
		if response != nil {
			_ = response.Body.Close()
		}
		requestDone <- err
	}()
	select {
	case <-handlerEntered:
	case <-time.After(2 * time.Second):
		close(allowHandler)
		t.Fatal("blocking handler did not start")
	}

	cancel()
	select {
	case err := <-serveDone:
		if err == nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("serve shutdown error = %v, want deadline exceeded", err)
		}
	case <-time.After(2 * time.Second):
		close(allowHandler)
		t.Fatal("server did not report forced shutdown")
	}
	select {
	case err := <-requestDone:
		if err == nil {
			t.Fatal("blocked request survived forced server close")
		}
	case <-time.After(2 * time.Second):
		close(allowHandler)
		t.Fatal("forced close did not terminate the blocked connection")
	}
	close(allowHandler)
}

func TestServerBindFailureDoesNotActivateServingAuthority(t *testing.T) {
	t.Parallel()
	held := listenServingAttestorTestTCP4(t)
	port := held.Addr().(*net.TCPAddr).Port
	attestor := NewServingAttestor()
	server := &Server{
		Config: &Config{
			Port:           port,
			ClaudeUpstream: "https://api.anthropic.com",
			LocalToken:     "test-token",
		},
		ServingAttestor: attestor,
	}
	if err := server.ListenAndServe(context.Background()); err == nil {
		t.Fatal("ListenAndServe succeeded while the exact port was occupied")
	}
	if _, err := attestor.Acquire(sha256.Sum256([]byte("binding"))); !errors.Is(err, ErrServingAttestorUnavailable) {
		t.Fatalf("Acquire after bind failure error = %v, want unavailable", err)
	}
}

func TestServerNilContextFailsBeforeServingAuthorityActivation(t *testing.T) {
	t.Parallel()
	listener := listenServingAttestorTestTCP4(t)
	attestor := NewServingAttestor()
	server := &Server{
		Config:          &Config{ClaudeUpstream: "https://api.anthropic.com", LocalToken: "test-token"},
		ServingAttestor: attestor,
	}
	if err := server.serve(nil, listener); err == nil {
		t.Fatal("serve accepted a nil context")
	}
	if _, err := attestor.Acquire(sha256.Sum256([]byte("binding"))); !errors.Is(err, ErrServingAttestorUnavailable) {
		t.Fatalf("Acquire after nil-context failure error = %v, want unavailable", err)
	}
}

func acquireServingProofTestLease(t *testing.T, attestor *ServingAttestor) ServingProofLease {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		lease, err := attestor.Acquire(sha256.Sum256([]byte("test binding")))
		if err == nil {
			return lease
		}
		if !errors.Is(err, ErrServingAttestorUnavailable) || time.Now().After(deadline) {
			t.Fatalf("Acquire error = %v", err)
		}
		time.Sleep(time.Millisecond)
	}
}

func requestServingProofTestHealth(t *testing.T, listener *net.TCPListener, challenge string) ([]byte, string, string, string) {
	t.Helper()
	var local, remote string
	request, err := http.NewRequest(http.MethodGet, "http://"+listener.Addr().String()+"/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	if challenge != "" {
		request.Header.Set(ServingProofChallengeHeader, challenge)
	}
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			local = info.Conn.LocalAddr().String()
			remote = info.Conn.RemoteAddr().String()
		},
	}))
	transport := &http.Transport{
		Proxy:              nil,
		DisableCompression: true,
		DisableKeepAlives:  true,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if network != "tcp" || address != listener.Addr().String() {
				return nil, errors.New("unexpected health dial target")
			}
			return (&net.Dialer{}).DialContext(ctx, "tcp4", address)
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		Timeout:   2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		t.Fatal(err)
	}
	return body, response.Header.Get(ServingProofResponseHeader), local, remote
}

type temporaryErrorListener struct {
	address net.Addr
}

func (l temporaryErrorListener) Accept() (net.Conn, error) {
	return nil, temporaryAcceptError{}
}

func (l temporaryErrorListener) Close() error { return nil }

func (l temporaryErrorListener) Addr() net.Addr { return l.address }

type temporaryAcceptError struct{}

func (temporaryAcceptError) Error() string   { return "temporary accept error" }
func (temporaryAcceptError) Timeout() bool   { return false }
func (temporaryAcceptError) Temporary() bool { return true }
