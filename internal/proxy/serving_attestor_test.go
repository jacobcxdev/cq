package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestServingAttestorProofBindsExactResponseAndConnection(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	random := make([]byte, servingAttestorSecretSize+servingAttestorEpochSize+servingAttestorNonceSize)
	for i := range random {
		random[i] = byte(i + 1)
	}
	attestor := newServingAttestor(bytes.NewReader(random), func() time.Time { return now }, time.Second, 4)
	listener := listenServingAttestorTestTCP4(t)
	if err := attestor.activate(listener); err != nil {
		t.Fatal(err)
	}
	binding := sha256.Sum256([]byte("ticket-owner-executable-binding"))
	lease, err := attestor.Acquire(binding)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()

	body := []byte("{\"status\":\"ok\"}\n")
	request := servingAttestorTestRequest(lease.Challenge(), listener.Addr(), "127.0.0.1:49152")
	proof, ok := attestor.ProveHealth(request, body)
	if !ok || proof == "" {
		t.Fatal("active attestor did not prove its issued challenge")
	}
	for _, test := range []struct {
		name         string
		body         []byte
		clientLocal  string
		clientRemote string
	}{
		{name: "body", body: []byte("{\"status\":\"degraded\"}\n"), clientLocal: "127.0.0.1:49152", clientRemote: listener.Addr().String()},
		{name: "local tuple", body: body, clientLocal: "127.0.0.1:49153", clientRemote: listener.Addr().String()},
		{name: "remote tuple", body: body, clientLocal: "127.0.0.1:49152", clientRemote: "127.0.0.1:19281"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := lease.VerifyResponse(test.body, proof, test.clientLocal, test.clientRemote); err == nil {
				t.Fatal("changed response authority verified")
			}
		})
	}
	if err := lease.VerifyResponse(body, proof, "127.0.0.1:49152", listener.Addr().String()); err != nil {
		t.Fatalf("VerifyResponse error = %v", err)
	}
	if err := lease.VerifyResponse(body, proof, "127.0.0.1:49152", listener.Addr().String()); !errors.Is(err, ErrServingProofInvalid) {
		t.Fatalf("replayed response error = %v, want invalid proof", err)
	}
	if err := lease.Seal(); err != nil {
		t.Fatalf("Seal error = %v", err)
	}
}

func TestServingAttestorListenerGenerationRequiresExactActivation(t *testing.T) {
	random := bytes.Repeat([]byte{0x42}, servingAttestorSecretSize+servingAttestorEpochSize)
	attestor := newServingAttestor(bytes.NewReader(random), time.Now, time.Second, 2)
	if _, err := attestor.ListenerGeneration(); !errors.Is(err, ErrServingAttestorUnavailable) {
		t.Fatalf("pre-activation generation error = %v", err)
	}
	listener := listenServingAttestorTestTCP4(t)
	if err := attestor.activate(listener); err != nil {
		t.Fatal(err)
	}
	first, err := attestor.ListenerGeneration()
	if err != nil {
		t.Fatal(err)
	}
	second, err := attestor.ListenerGeneration()
	if err != nil || first != second || first == ([sha256.Size]byte{}) {
		t.Fatalf("listener generation = %x, %x, error %v", first, second, err)
	}
	<-attestor.BeginClose()
	if _, err := attestor.ListenerGeneration(); !errors.Is(err, ErrServingAttestorUnavailable) {
		t.Fatalf("closed generation error = %v", err)
	}
}

func TestServingResponseMACVector(t *testing.T) {
	t.Parallel()
	var secret [servingAttestorSecretSize]byte
	var epoch [servingAttestorEpochSize]byte
	for index := range secret {
		secret[index] = byte(index + 1)
		epoch[index] = byte(0xff - index)
	}
	binding := sha256.Sum256([]byte("binding vector"))
	challenge := "AQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eHyA"
	local := "127.0.0.1:19280"
	remote := "127.0.0.1:49152"
	body := []byte("{\"status\":\"ok\"}\n")
	proof := servingResponseMAC(secret, challenge, binding, epoch, local, remote, body)
	const want = "f18a0ec49b4e6492f95899500365a203ff0469a219a78aae66b28b267c0d9251"
	if got := hex.EncodeToString(proof[:]); got != want {
		t.Fatalf("serving proof vector = %s, want %s", got, want)
	}
	assertDifferent := func(name, candidateChallenge string, candidateBinding [sha256.Size]byte, candidateEpoch [servingAttestorEpochSize]byte, candidateLocal, candidateRemote string, candidateBody []byte) {
		t.Helper()
		if candidate := servingResponseMAC(secret, candidateChallenge, candidateBinding, candidateEpoch, candidateLocal, candidateRemote, candidateBody); candidate == proof {
			t.Fatalf("%s did not change the serving proof", name)
		}
	}
	assertDifferent("challenge", challenge+"A", binding, epoch, local, remote, body)
	changedBinding := binding
	changedBinding[0] ^= 1
	assertDifferent("binding", challenge, changedBinding, epoch, local, remote, body)
	changedEpoch := epoch
	changedEpoch[0] ^= 1
	assertDifferent("epoch", challenge, binding, changedEpoch, local, remote, body)
	assertDifferent("local", challenge, binding, epoch, "127.0.0.1:19281", remote, body)
	assertDifferent("remote", challenge, binding, epoch, local, "127.0.0.1:49153", body)
	assertDifferent("body", challenge, binding, epoch, local, remote, append(append([]byte(nil), body...), ' '))
}

func TestServingAttestorChallengeIsOneShot(t *testing.T) {
	t.Parallel()
	attestor := newServingAttestor(
		bytes.NewReader(bytes.Repeat([]byte{1}, servingAttestorSecretSize+servingAttestorEpochSize+servingAttestorNonceSize)),
		time.Now, time.Second, 4,
	)
	listener := listenServingAttestorTestTCP4(t)
	if err := attestor.activate(listener); err != nil {
		t.Fatal(err)
	}
	lease, err := attestor.Acquire(sha256.Sum256([]byte("binding")))
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	request := servingAttestorTestRequest(lease.Challenge(), listener.Addr(), "127.0.0.1:49152")
	if _, ok := attestor.ProveHealth(request, []byte("health")); !ok {
		t.Fatal("first proof failed")
	}
	if proof, ok := attestor.ProveHealth(request, []byte("health")); ok || proof != "" {
		t.Fatal("replayed challenge produced a proof")
	}
}

func TestServingAttestorBeginCloseRejectsNewAndWaitsForLease(t *testing.T) {
	t.Parallel()
	attestor := newServingAttestor(
		bytes.NewReader(bytes.Repeat([]byte{2}, servingAttestorSecretSize+servingAttestorEpochSize+servingAttestorNonceSize*2)),
		time.Now, time.Second, 4,
	)
	listener := listenServingAttestorTestTCP4(t)
	if err := attestor.activate(listener); err != nil {
		t.Fatal(err)
	}
	lease, err := attestor.Acquire(sha256.Sum256([]byte("binding")))
	if err != nil {
		t.Fatal(err)
	}
	closeDone := attestor.BeginClose()
	select {
	case <-closeDone:
		t.Fatal("BeginClose returned while a serving lease was held")
	case <-time.After(20 * time.Millisecond):
	}
	if _, err := attestor.Acquire(sha256.Sum256([]byte("second binding"))); err == nil {
		t.Fatal("Acquire succeeded after BeginClose linearised")
	}
	lease.Release()
	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("BeginClose did not return after the serving lease released")
	}
	lease.Release()
}

func TestServingAttestorCopiesShareOneLifecycle(t *testing.T) {
	t.Parallel()
	attestor := newServingAttestor(
		bytes.NewReader(bytes.Repeat([]byte{3}, servingAttestorSecretSize+servingAttestorEpochSize+servingAttestorNonceSize*2)),
		time.Now, time.Second, 4,
	)
	listener := listenServingAttestorTestTCP4(t)
	if err := attestor.activate(listener); err != nil {
		t.Fatal(err)
	}
	copyOfAttestor := *attestor
	lease, err := copyOfAttestor.Acquire(sha256.Sum256([]byte("binding")))
	if err != nil {
		t.Fatal(err)
	}
	closeDone := attestor.BeginClose()
	if _, err := copyOfAttestor.Acquire(sha256.Sum256([]byte("second binding"))); err == nil {
		t.Fatal("copied attestor acquired after the shared lifecycle began closing")
	}
	select {
	case <-closeDone:
		t.Fatal("shared lifecycle closed while a copied handle held a lease")
	default:
	}
	lease.Release()
	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("shared lifecycle did not close after copied handle released")
	}
}

func TestServingAttestorCopiedLeaseReleaseSharesOneEffect(t *testing.T) {
	t.Parallel()
	random := make([]byte, servingAttestorSecretSize+servingAttestorEpochSize+servingAttestorNonceSize*2)
	for index := range random {
		random[index] = byte(index + 1)
	}
	attestor := newServingAttestor(
		bytes.NewReader(random),
		time.Now, time.Second, 4,
	)
	listener := listenServingAttestorTestTCP4(t)
	if err := attestor.activate(listener); err != nil {
		t.Fatal(err)
	}
	first, err := attestor.Acquire(sha256.Sum256([]byte("first binding")))
	if err != nil {
		t.Fatal(err)
	}
	second, err := attestor.Acquire(sha256.Sum256([]byte("second binding")))
	if err != nil {
		t.Fatal(err)
	}
	firstCopy := first
	closeDone := attestor.BeginClose()
	var releases sync.WaitGroup
	releases.Add(2)
	go func() { defer releases.Done(); first.Release() }()
	go func() { defer releases.Done(); firstCopy.Release() }()
	releases.Wait()
	select {
	case <-closeDone:
		t.Fatal("copied lease release consumed the second lease")
	default:
	}
	second.Release()
	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("shared lifecycle did not close after both distinct leases released")
	}
}

func TestServingAttestorReleasedLeaseCannotVerify(t *testing.T) {
	t.Parallel()
	random := make([]byte, servingAttestorSecretSize+servingAttestorEpochSize+servingAttestorNonceSize*2)
	for i := range random {
		random[i] = byte(i + 1)
	}
	attestor := newServingAttestor(
		bytes.NewReader(random),
		time.Now, time.Second, 4,
	)
	listener := listenServingAttestorTestTCP4(t)
	if err := attestor.activate(listener); err != nil {
		t.Fatal(err)
	}
	lease, err := attestor.Acquire(sha256.Sum256([]byte("binding")))
	if err != nil {
		t.Fatal(err)
	}
	other, err := attestor.Acquire(sha256.Sum256([]byte("other binding")))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Release()
	body := []byte("health")
	request := servingAttestorTestRequest(lease.Challenge(), listener.Addr(), "127.0.0.1:49152")
	proof, ok := attestor.ProveHealth(request, body)
	if !ok {
		t.Fatal("issued challenge was not proved")
	}
	lease.Release()
	if err := lease.VerifyResponse(body, proof, "127.0.0.1:49152", listener.Addr().String()); err == nil {
		t.Fatal("released lease retained proof authority")
	}
}

func TestServingAttestorExpiresPendingChallengesAndBoundsCapacity(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	random := make([]byte, servingAttestorSecretSize+servingAttestorEpochSize+servingAttestorNonceSize*3)
	for i := range random {
		random[i] = byte(i + 1)
	}
	attestor := newServingAttestor(bytes.NewReader(random), func() time.Time { return now }, time.Second, 1)
	listener := listenServingAttestorTestTCP4(t)
	if err := attestor.activate(listener); err != nil {
		t.Fatal(err)
	}
	first, err := attestor.Acquire(sha256.Sum256([]byte("first")))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	if _, err := attestor.Acquire(sha256.Sum256([]byte("capacity"))); !errors.Is(err, ErrServingProofCapacity) {
		t.Fatalf("capacity Acquire error = %v", err)
	}
	now = now.Add(time.Second)
	request := servingAttestorTestRequest(first.Challenge(), listener.Addr(), "127.0.0.1:49152")
	if proof, ok := attestor.ProveHealth(request, []byte("health")); ok || proof != "" {
		t.Fatal("expired challenge produced a proof")
	}
	second, err := attestor.Acquire(sha256.Sum256([]byte("second")))
	if err != nil {
		t.Fatalf("Acquire after expiry error = %v", err)
	}
	body := []byte("fresh health")
	request = servingAttestorTestRequest(second.Challenge(), listener.Addr(), "127.0.0.1:49152")
	proof, ok := attestor.ProveHealth(request, body)
	if !ok {
		t.Fatal("fresh challenge was not proved")
	}
	now = now.Add(time.Second)
	if err := second.VerifyResponse(body, proof, "127.0.0.1:49152", listener.Addr().String()); !errors.Is(err, ErrServingProofInvalid) {
		t.Fatalf("expired response error = %v, want invalid proof", err)
	}
	if err := second.Seal(); !errors.Is(err, ErrServingProofInvalid) {
		t.Fatalf("Seal after expired response error = %v, want invalid proof", err)
	}
	second.Release()
	third, err := attestor.Acquire(sha256.Sum256([]byte("third")))
	if err != nil {
		t.Fatalf("third Acquire error = %v", err)
	}
	request = servingAttestorTestRequest(third.Challenge(), listener.Addr(), "127.0.0.1:49152")
	proof, ok = attestor.ProveHealth(request, body)
	if !ok {
		t.Fatal("third challenge was not proved")
	}
	if err := third.VerifyResponse(body, proof, "127.0.0.1:49152", listener.Addr().String()); err != nil {
		t.Fatalf("third VerifyResponse error = %v", err)
	}
	now = now.Add(time.Second)
	if err := third.Seal(); !errors.Is(err, ErrServingProofInvalid) {
		t.Fatalf("Seal after verified response expiry error = %v, want invalid proof", err)
	}
	attestor.state.mu.Lock()
	sealedLeases := attestor.state.sealedLeases
	attestor.state.mu.Unlock()
	if sealedLeases != 0 {
		t.Fatalf("expired Seal retained %d sealed leases", sealedLeases)
	}
	third.Release()
}

func TestServingAttestorRejectsNonTCP4LoopbackListeners(t *testing.T) {
	t.Parallel()
	t.Run("wildcard tcp4", func(t *testing.T) {
		listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4zero})
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		if err := NewServingAttestor().activate(listener); !errors.Is(err, ErrServingAttestorUnavailable) {
			t.Fatalf("Activate wildcard TCP4 error = %v, want unavailable", err)
		}
	})
	t.Run("tcp6", func(t *testing.T) {
		listener, err := net.ListenTCP("tcp6", &net.TCPAddr{IP: net.IPv6loopback})
		if err != nil {
			t.Skipf("TCP6 loopback unavailable: %v", err)
		}
		defer listener.Close()
		if err := NewServingAttestor().activate(listener); !errors.Is(err, ErrServingAttestorUnavailable) {
			t.Fatalf("Activate TCP6 error = %v, want unavailable", err)
		}
	})
}

func TestServingAttestorBeginCloseInvalidatesAuthority(t *testing.T) {
	t.Parallel()
	attestor := NewServingAttestor()
	listener := listenServingAttestorTestTCP4(t)
	if err := attestor.activate(listener); err != nil {
		t.Fatal(err)
	}
	select {
	case <-attestor.BeginClose():
	case <-time.After(2 * time.Second):
		t.Fatal("BeginClose without leases did not complete")
	}
	attestor.state.mu.Lock()
	defer attestor.state.mu.Unlock()
	if attestor.state.active || attestor.state.listenerAddr != "" || len(attestor.state.pending) != 0 ||
		attestor.state.secret != ([servingAttestorSecretSize]byte{}) || attestor.state.epoch != ([servingAttestorEpochSize]byte{}) {
		t.Fatal("BeginClose retained serving authority")
	}
}

func TestServingAttestorUnexpectedAbortBeforeActivationFailsClosed(t *testing.T) {
	t.Parallel()
	attestor := NewServingAttestor()
	select {
	case <-attestor.abortUnexpected():
	case <-time.After(2 * time.Second):
		t.Fatal("inactive unexpected abort did not complete")
	}
	if lease, err := attestor.Acquire(sha256.Sum256([]byte("late binding"))); lease != nil || !errors.Is(err, ErrServingAttestorClosing) {
		t.Fatalf("Acquire after inactive abort = %v, %v", lease, err)
	}
}

func TestServingAttestorFailsClosedOnShortEntropyAndNonceCollision(t *testing.T) {
	t.Parallel()
	t.Run("activation entropy", func(t *testing.T) {
		attestor := newServingAttestor(bytes.NewReader(make([]byte, servingAttestorSecretSize)), time.Now, time.Second, 1)
		if err := attestor.activate(listenServingAttestorTestTCP4(t)); !errors.Is(err, ErrServingAttestorUnavailable) {
			t.Fatalf("Activate error = %v", err)
		}
	})
	t.Run("nonce collision", func(t *testing.T) {
		random := bytes.Repeat([]byte{7}, servingAttestorSecretSize+servingAttestorEpochSize+servingAttestorNonceSize*2)
		attestor := newServingAttestor(bytes.NewReader(random), time.Now, time.Second, 2)
		if err := attestor.activate(listenServingAttestorTestTCP4(t)); err != nil {
			t.Fatal(err)
		}
		lease, err := attestor.Acquire(sha256.Sum256([]byte("first")))
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Release()
		if _, err := attestor.Acquire(sha256.Sum256([]byte("second"))); !errors.Is(err, ErrServingAttestorUnavailable) {
			t.Fatalf("colliding Acquire error = %v", err)
		}
	})
	t.Run("short nonce", func(t *testing.T) {
		random := bytes.Repeat([]byte{9}, servingAttestorSecretSize+servingAttestorEpochSize+servingAttestorNonceSize-1)
		attestor := newServingAttestor(bytes.NewReader(random), time.Now, time.Second, 2)
		if err := attestor.activate(listenServingAttestorTestTCP4(t)); err != nil {
			t.Fatal(err)
		}
		if lease, err := attestor.Acquire(sha256.Sum256([]byte("short"))); lease != nil || !errors.Is(err, ErrServingAttestorUnavailable) {
			t.Fatalf("short nonce Acquire = %v, %v", lease, err)
		}
		attestor.state.mu.Lock()
		leases, pending := attestor.state.leases, len(attestor.state.pending)
		attestor.state.mu.Unlock()
		if leases != 0 || pending != 0 {
			t.Fatalf("short nonce leaked leases=%d pending=%d", leases, pending)
		}
		select {
		case <-attestor.BeginClose():
		case <-time.After(2 * time.Second):
			t.Fatal("short nonce failure blocked close")
		}
	})
}

func TestServingAttestorConcurrentChallengeConsumptionIsOneShot(t *testing.T) {
	t.Parallel()
	attestor := newServingAttestor(
		bytes.NewReader(bytes.Repeat([]byte{8}, servingAttestorSecretSize+servingAttestorEpochSize+servingAttestorNonceSize)),
		time.Now, time.Second, 2,
	)
	listener := listenServingAttestorTestTCP4(t)
	if err := attestor.activate(listener); err != nil {
		t.Fatal(err)
	}
	lease, err := attestor.Acquire(sha256.Sum256([]byte("binding")))
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	request := servingAttestorTestRequest(lease.Challenge(), listener.Addr(), "127.0.0.1:49152")
	var successes atomic.Int32
	var wait sync.WaitGroup
	for range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, ok := attestor.ProveHealth(request, []byte("health")); ok {
				successes.Add(1)
			}
		}()
	}
	wait.Wait()
	if got := successes.Load(); got != 1 {
		t.Fatalf("successful proof count = %d, want 1", got)
	}
}

func listenServingAttestorTestTCP4(t *testing.T) *net.TCPListener {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

func servingAttestorTestRequest(challenge string, local net.Addr, remote string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/health", nil)
	request.Header.Set(ServingProofChallengeHeader, challenge)
	request.RemoteAddr = remote
	return request.WithContext(context.WithValue(request.Context(), http.LocalAddrContextKey, local))
}
