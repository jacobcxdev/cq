package proxy

import (
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestCaptureCodexInstalledExecutableRejectsSymlinkRetarget(t *testing.T) {
	directory := t.TempDir()
	first := filepath.Join(directory, "first")
	second := filepath.Join(directory, "second")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, []byte("executable"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	_, err := captureCodexInstalledExecutableWithResolver(filepath.Join(directory, "client"), func(string) (string, error) {
		calls++
		if calls == 1 {
			return first, nil
		}
		return second, nil
	})
	if !errors.Is(err, errCodexInstalledProcessAttestation) {
		t.Fatalf("retargeted executable capture = %v", err)
	}
}

func TestCodexInstalledExecutableProofRejectsSpecialModes(t *testing.T) {
	proof := testCodexInstalledExecutableProof("/Applications/Codex.app/Contents/Resources/codex", "client")
	for _, mode := range []os.FileMode{os.ModeSetuid, os.ModeSetgid, os.ModeSticky} {
		changed := proof
		changed.mode |= mode
		if changed.valid() {
			t.Fatalf("executable proof accepted special mode %v", mode)
		}
	}
}

func TestCodexInstalledProcessAuthorityBindsAndRevalidatesExactProcess(t *testing.T) {
	listenerAddress, attestor := startCodexInstalledAttestedHealthServer(t)
	clientProof := testCodexInstalledExecutableProof("/Applications/Codex.app/Contents/Resources/codex", "client")
	processProof := testCodexInstalledPlatformProof("cq")
	platform := &testCodexInstalledPlatformVerifier{proof: processProof}
	clientCapture := &testCodexInstalledExecutableCapture{proof: clientProof}
	config := codexInstalledListenerProcessAuthorityConfig{
		cqBuild:          "cq-1.2.3",
		clientBuild:      "0.146.0",
		clientExecutable: clientProof.path,
		listenerAddress:  listenerAddress,
		servingAttestor:  attestor,
		nativeHTTP:       &CodexNativeHTTPHandler{},
	}
	authority, err := newCodexInstalledListenerProcessAuthorityWithDependencies(
		context.Background(),
		config,
		codexInstalledProcessAttestationDependencies{
			platform:          platform,
			captureExecutable: clientCapture.capture,
			runVersion: func(_ context.Context, path string, _ codexInstalledExecutableProof) ([]byte, error) {
				if path != clientProof.path {
					return nil, errors.New("wrong executable")
				}
				return []byte("codex-cli 0.146.0\n"), nil
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	tuple := testCodexInstalledProcessTuple()
	lease, err := authority.Acquire(context.Background(), tuple)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	binding := lease.Binding()
	if binding.CQBuild != tuple.CQBuild || binding.ClientBuild != tuple.ClientBuild ||
		binding.PID != processProof.pid || binding.ServiceKind != processProof.serviceKind || !binding.Persistent ||
		binding.ExecutableSHA256 != processProof.executable.sha256 ||
		binding.ClientExecutableSHA256 != clientProof.sha256 ||
		binding.ServiceIdentitySHA256 != processProof.serviceIdentitySHA256 ||
		binding.ListenerBinding == ([sha256.Size]byte{}) {
		t.Fatalf("binding = %#v", binding)
	}
	if _, err := lease.Snapshot(context.Background()); err != nil {
		t.Fatalf("snapshot exact probe: %v", err)
	}
	if err := lease.Revalidate(context.Background()); err != nil {
		t.Fatalf("revalidate exact process: %v", err)
	}
	if build, err := authority.Probe(context.Background()); err != nil || build != tuple.ClientBuild {
		t.Fatalf("client build = %q, %v", build, err)
	}

	changedProcess := processProof
	changedProcess.serviceIdentitySHA256 = sha256.Sum256([]byte("changed service"))
	platform.set(changedProcess)
	if err := lease.Revalidate(context.Background()); !errors.Is(err, errCodexInstalledProcessAttestation) {
		t.Fatalf("changed service revalidation = %v", err)
	}
	changedProcess = processProof
	changedProcess.executable.sha256 = sha256.Sum256([]byte("changed CQ executable"))
	platform.set(changedProcess)
	if err := lease.Revalidate(context.Background()); !errors.Is(err, errCodexInstalledProcessAttestation) {
		t.Fatalf("changed CQ executable revalidation = %v", err)
	}
	platform.set(processProof)
	changedClient := clientProof
	changedClient.sha256 = sha256.Sum256([]byte("changed client"))
	clientCapture.set(changedClient)
	if build, err := authority.Probe(context.Background()); build != "" || !errors.Is(err, errCodexInstalledProcessAttestation) {
		t.Fatalf("changed client probe = %q, %v", build, err)
	}
	if err := lease.Revalidate(context.Background()); !errors.Is(err, errCodexInstalledProcessAttestation) {
		t.Fatalf("changed client revalidation = %v", err)
	}

	lease.Release()
	replacement, err := newCodexInstalledHTTPGateProbe(binding.ListenerBinding)
	if err != nil {
		t.Fatal(err)
	}
	detach, err := config.nativeHTTP.installCodexInstalledHTTPGateProbe(replacement)
	if err != nil {
		t.Fatalf("released lease retained native probe: %v", err)
	}
	detach()
}

func TestCodexInstalledProcessAuthorityReleasesServingLeaseWhenProbeInstallPanics(t *testing.T) {
	listenerAddress, attestor := startCodexInstalledAttestedHealthServer(t)
	clientProof := testCodexInstalledExecutableProof("/opt/homebrew/bin/codex", "client")
	processProof := testCodexInstalledPlatformProof("cq")
	authority, err := newCodexInstalledListenerProcessAuthorityWithDependencies(
		context.Background(),
		codexInstalledListenerProcessAuthorityConfig{
			cqBuild:          "cq-1.2.3",
			clientBuild:      "0.146.0",
			clientExecutable: clientProof.path,
			listenerAddress:  listenerAddress,
			servingAttestor:  attestor,
			nativeHTTP:       &CodexNativeHTTPHandler{},
		},
		codexInstalledProcessAttestationDependencies{
			platform:          &testCodexInstalledPlatformVerifier{proof: processProof},
			captureExecutable: (&testCodexInstalledExecutableCapture{proof: clientProof}).capture,
			runVersion: func(context.Context, string, codexInstalledExecutableProof) ([]byte, error) {
				return []byte("codex-cli 0.146.0\n"), nil
			},
			installProbe: func(*CodexNativeHTTPHandler, *codexInstalledHTTPGateProbe) (func(), error) {
				panic("probe install panic")
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("Acquire did not propagate probe install panic")
			}
		}()
		_, _ = authority.Acquire(context.Background(), testCodexInstalledProcessTuple())
	}()
	attestor.state.mu.Lock()
	sealedLeases := attestor.state.sealedLeases
	attestor.state.mu.Unlock()
	if sealedLeases != 0 {
		t.Fatalf("sealed serving leases = %d, want 0 after panic", sealedLeases)
	}
}

func TestCodexInstalledProcessAuthorityRejectsTupleAndLeaseNearMatches(t *testing.T) {
	listenerAddress, attestor := startCodexInstalledAttestedHealthServer(t)
	clientProof := testCodexInstalledExecutableProof("/opt/homebrew/bin/codex", "client")
	processProof := testCodexInstalledPlatformProof("cq")
	authority, err := newCodexInstalledListenerProcessAuthorityWithDependencies(
		context.Background(),
		codexInstalledListenerProcessAuthorityConfig{
			cqBuild:          "cq-1.2.3",
			clientBuild:      "0.146.0",
			clientExecutable: clientProof.path,
			listenerAddress:  listenerAddress,
			servingAttestor:  attestor,
			nativeHTTP:       &CodexNativeHTTPHandler{},
		},
		codexInstalledProcessAttestationDependencies{
			platform:          &testCodexInstalledPlatformVerifier{proof: processProof},
			captureExecutable: (&testCodexInstalledExecutableCapture{proof: clientProof}).capture,
			runVersion: func(context.Context, string, codexInstalledExecutableProof) ([]byte, error) {
				return []byte("codex-cli 0.146.0\n"), nil
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	tuple := testCodexInstalledProcessTuple()
	wrongBuild := tuple
	wrongBuild.CQBuild = "cq-other"
	if lease, err := authority.Acquire(context.Background(), wrongBuild); lease != nil || !errors.Is(err, errCodexInstalledProcessAttestation) {
		t.Fatalf("wrong CQ tuple acquired lease: %#v, %v", lease, err)
	}
	wrongClient := tuple
	wrongClient.ClientBuild = "0.145.0"
	if lease, err := authority.Acquire(context.Background(), wrongClient); lease != nil || !errors.Is(err, errCodexInstalledProcessAttestation) {
		t.Fatalf("wrong client tuple acquired lease: %#v, %v", lease, err)
	}
	lease, err := authority.Acquire(context.Background(), tuple)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if duplicate, err := authority.Acquire(context.Background(), tuple); duplicate != nil || !errors.Is(err, errCodexInstalledProcessAttestation) {
		t.Fatalf("concurrent acquisition = %#v, %v", duplicate, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := lease.Snapshot(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled snapshot = %v", err)
	}
}

func TestCodexInstalledClientBuildProbeRequiresExactBinaryAndOutput(t *testing.T) {
	proof := testCodexInstalledExecutableProof("/opt/homebrew/bin/codex", "client")
	for _, test := range []struct {
		name   string
		output []byte
		want   string
	}{
		{name: "release", output: []byte("codex-cli 0.146.0\n"), want: "0.146.0"},
		{name: "prerelease", output: []byte("codex-cli 0.147.0-beta.3\n"), want: "0.147.0-beta.3"},
	} {
		t.Run(test.name, func(t *testing.T) {
			probe, err := newCodexInstalledClientExecutableBuildProbe(
				context.Background(), proof.path, test.want,
				func(string) (codexInstalledExecutableProof, error) { return proof, nil },
				func(context.Context, string, codexInstalledExecutableProof) ([]byte, error) { return test.output, nil },
			)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := probe.Probe(context.Background()); err != nil || got != test.want {
				t.Fatalf("Probe() = %q, %v", got, err)
			}
		})
	}

	for _, output := range [][]byte{
		[]byte("0.146.0\n"),
		[]byte("prefix codex-cli 0.146.0\n"),
		[]byte("codex-cli 0.146.0 trailing\n"),
		[]byte("codex-cli 0.146.0\nsecond line\n"),
		append([]byte("codex-cli 0.146.0"), make([]byte, codexInstalledVersionOutputMaxBytes)...),
	} {
		if probe, err := newCodexInstalledClientExecutableBuildProbe(
			context.Background(), proof.path, "0.146.0",
			func(string) (codexInstalledExecutableProof, error) { return proof, nil },
			func(context.Context, string, codexInstalledExecutableProof) ([]byte, error) { return output, nil },
		); probe != nil || !errors.Is(err, errCodexInstalledProcessAttestation) {
			t.Fatalf("near-match output created probe: %#v, %v", probe, err)
		}
	}
}

func TestCodexInstalledClientBuildProbeRejectsReplacementDuringVersionProbe(t *testing.T) {
	baseline := testCodexInstalledExecutableProof("/opt/homebrew/bin/codex", "client")
	replacement := baseline
	replacement.sha256 = sha256.Sum256([]byte("replacement"))
	captures := 0
	probe, err := newCodexInstalledClientExecutableBuildProbe(
		context.Background(), baseline.path, "0.146.0",
		func(string) (codexInstalledExecutableProof, error) {
			captures++
			if captures >= 3 {
				return replacement, nil
			}
			return baseline, nil
		},
		func(context.Context, string, codexInstalledExecutableProof) ([]byte, error) {
			return []byte("codex-cli 0.146.0\n"), nil
		},
	)
	if probe != nil || !errors.Is(err, errCodexInstalledProcessAttestation) {
		t.Fatalf("replacement during version probe = %#v, %v", probe, err)
	}
}

func startCodexInstalledAttestedHealthServer(t *testing.T) (string, *ServingAttestor) {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	attestor := NewServingAttestor()
	servingListener, err := attestor.ActivateListener(listener)
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	body := []byte("{\"status\":\"ok\"}\n")
	server := &http.Server{
		ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			if proof, ok := attestor.ProveHealth(request, body); ok {
				writer.Header().Set(ServingProofResponseHeader, proof)
			}
			_, _ = writer.Write(body)
		}),
	}
	done := make(chan struct{})
	go func() {
		_ = server.Serve(servingListener)
		close(done)
	}()
	t.Cleanup(func() {
		_ = server.Close()
		<-done
		select {
		case <-attestor.BeginClose():
		case <-time.After(time.Second):
			t.Error("serving attestor did not close")
		}
	})
	return listener.Addr().String(), attestor
}

type testCodexInstalledPlatformVerifier struct {
	mu    sync.Mutex
	proof codexInstalledProcessPlatformProof
	err   error
}

func (verifier *testCodexInstalledPlatformVerifier) Capture(context.Context) (codexInstalledProcessPlatformProof, error) {
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	return verifier.proof, verifier.err
}

func (verifier *testCodexInstalledPlatformVerifier) set(proof codexInstalledProcessPlatformProof) {
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	verifier.proof = proof
}

type testCodexInstalledExecutableCapture struct {
	mu    sync.Mutex
	proof codexInstalledExecutableProof
}

func (capture *testCodexInstalledExecutableCapture) capture(string) (codexInstalledExecutableProof, error) {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	return capture.proof, nil
}

func (capture *testCodexInstalledExecutableCapture) set(proof codexInstalledExecutableProof) {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	capture.proof = proof
}

func testCodexInstalledExecutableProof(path, material string) codexInstalledExecutableProof {
	return codexInstalledExecutableProof{
		path:   path,
		device: 1,
		inode:  2,
		links:  1,
		owner:  uint64(os.Geteuid()),
		size:   int64(len(material)),
		mode:   0o755,
		sha256: sha256.Sum256([]byte(material)),
	}
}

func testCodexInstalledPlatformProof(material string) codexInstalledProcessPlatformProof {
	return codexInstalledProcessPlatformProof{
		pid:                   os.Getpid(),
		serviceKind:           codexInstalledListenerServiceLaunchd,
		persistent:            true,
		executable:            testCodexInstalledExecutableProof("/opt/homebrew/bin/cq", material),
		serviceIdentitySHA256: sha256.Sum256([]byte("service-" + material)),
	}
}

func testCodexInstalledProcessTuple() CodexReadinessTuple {
	return CodexReadinessTuple{
		Transport:         CodexRoutingHTTP,
		CQBuild:           "cq-1.2.3",
		ParserSchema:      4,
		LeaseSchema:       2,
		SemanticsRevision: "semantics-v1",
		ClientBuild:       "0.146.0",
		RetryBudget:       1,
		FixtureHash:       "fixture",
	}
}
