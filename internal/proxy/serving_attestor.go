package proxy

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"
)

const (
	ServingProofChallengeHeader = "X-CQ-Serving-Challenge"
	ServingProofResponseHeader  = "X-CQ-Serving-Proof"

	servingAttestorSecretSize = 32
	servingAttestorEpochSize  = 32
	servingAttestorNonceSize  = 32

	defaultServingProofTTL          = 5 * time.Second
	defaultServingPendingLimit      = 32
	servingListenerGenerationDomain = "cq/serving-listener-generation/v1\x00"
)

var (
	ErrServingAttestorUnavailable = errors.New("serving attestor unavailable")
	ErrServingAttestorClosing     = errors.New("serving attestor closing")
	ErrServingProofCapacity       = errors.New("serving proof capacity exhausted")
	ErrServingProofInvalid        = errors.New("serving proof invalid")
)

type servingPendingChallenge struct {
	binding [sha256.Size]byte
	epoch   [servingAttestorEpochSize]byte
	expires time.Time
}

// ServingAttestor binds one-shot health proofs and serving leases to one exact
// loopback TCP listener generation.
type ServingAttestor struct {
	state *servingAttestorState
}

type servingAttestorState struct {
	mu sync.Mutex

	random       io.Reader
	now          func() time.Time
	ttl          time.Duration
	pendingLimit int

	active       bool
	closing      bool
	aborted      bool
	listenerAddr string
	secret       [servingAttestorSecretSize]byte
	epoch        [servingAttestorEpochSize]byte
	pending      map[string]servingPendingChallenge
	leases       int
	sealedLeases int
	closeDone    chan struct{}
	closeDoneSet bool
	abortDone    chan struct{}
	abortDoneSet bool
}

// ServingProofLease retains an activated listener generation until Release.
type ServingProofLease interface {
	Challenge() string
	VerifyResponse(body []byte, encodedProof, clientLocal, clientRemote string) error
	Seal() error
	Release()
}

type servingProofLease struct {
	state *servingProofLeaseState
}

type servingProofLeaseState struct {
	attestor  *servingAttestorState
	challenge string
	binding   [sha256.Size]byte
	epoch     [servingAttestorEpochSize]byte
	expires   time.Time
	once      sync.Once
	released  bool
	verified  bool
	sealed    bool
}

// NewServingAttestor constructs an inactive attestor. ActivateListener must
// receive the exact TCP listener later delegated to http.Server.Serve.
func NewServingAttestor() *ServingAttestor {
	return newServingAttestor(rand.Reader, time.Now, defaultServingProofTTL, defaultServingPendingLimit)
}

func newServingAttestor(random io.Reader, now func() time.Time, ttl time.Duration, pendingLimit int) *ServingAttestor {
	return &ServingAttestor{state: &servingAttestorState{
		random: random, now: now, ttl: ttl, pendingLimit: pendingLimit,
	}}
}

// activate creates fresh proof authority for an already-bound IPv4 loopback
// listener. Serving callers must use ActivateListener so fatal Accept errors
// cannot bypass teardown linearisation.
func (a *ServingAttestor) activate(listener *net.TCPListener) error {
	if a == nil || a.state == nil || listener == nil {
		return ErrServingAttestorUnavailable
	}
	address, err := canonicalServingTCP4Address(listener.Addr().String())
	if err != nil {
		return ErrServingAttestorUnavailable
	}
	state := a.state
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.active || state.closing || state.random == nil || state.now == nil || state.ttl <= 0 || state.pendingLimit <= 0 {
		return ErrServingAttestorUnavailable
	}
	var secret [servingAttestorSecretSize]byte
	var epoch [servingAttestorEpochSize]byte
	if _, err := io.ReadFull(state.random, secret[:]); err != nil {
		return ErrServingAttestorUnavailable
	}
	if _, err := io.ReadFull(state.random, epoch[:]); err != nil {
		return ErrServingAttestorUnavailable
	}
	state.active = true
	state.listenerAddr = address
	state.secret = secret
	state.epoch = epoch
	state.pending = make(map[string]servingPendingChallenge)
	state.closeDone = make(chan struct{})
	state.closeDoneSet = false
	state.abortDone = make(chan struct{})
	state.abortDoneSet = false
	return nil
}

// ActivateListener creates fresh proof authority and returns the only listener
// wrapper that should be passed to http.Server.Serve. The wrapper delegates the
// exact pre-bound TCP4 listener and revokes unsealed authority before a fatal
// Accept error is surfaced.
func (a *ServingAttestor) ActivateListener(listener *net.TCPListener) (net.Listener, error) {
	if err := a.activate(listener); err != nil {
		return nil, err
	}
	return &servingAttestedTCP4Listener{Listener: listener, attestor: a}, nil
}

// ListenerGeneration returns a stable, non-secret identifier for the exact
// activated listener generation. It is unavailable before activation and once
// teardown begins.
func (a *ServingAttestor) ListenerGeneration() ([sha256.Size]byte, error) {
	if a == nil || a.state == nil {
		return [sha256.Size]byte{}, ErrServingAttestorUnavailable
	}
	state := a.state
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.active || state.closing || state.aborted || state.listenerAddr == "" {
		return [sha256.Size]byte{}, ErrServingAttestorUnavailable
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(servingListenerGenerationDomain))
	_, _ = hash.Write([]byte(state.listenerAddr))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(state.epoch[:])
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest, nil
}

// Acquire reserves one one-shot challenge and holds the listener generation
// open until the returned lease is released.
func (a *ServingAttestor) Acquire(binding [sha256.Size]byte) (ServingProofLease, error) {
	if a == nil || a.state == nil || binding == ([sha256.Size]byte{}) {
		return nil, ErrServingAttestorUnavailable
	}
	state := a.state
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closing {
		return nil, ErrServingAttestorClosing
	}
	if !state.active || state.random == nil || state.now == nil {
		return nil, ErrServingAttestorUnavailable
	}
	now := state.now()
	for challenge, pending := range state.pending {
		if !now.Before(pending.expires) {
			delete(state.pending, challenge)
		}
	}
	if len(state.pending) >= state.pendingLimit {
		return nil, ErrServingProofCapacity
	}
	var nonce [servingAttestorNonceSize]byte
	if _, err := io.ReadFull(state.random, nonce[:]); err != nil {
		return nil, ErrServingAttestorUnavailable
	}
	challenge := base64.RawURLEncoding.EncodeToString(nonce[:])
	if _, exists := state.pending[challenge]; exists {
		return nil, ErrServingAttestorUnavailable
	}
	expires := now.Add(state.ttl)
	state.pending[challenge] = servingPendingChallenge{binding: binding, epoch: state.epoch, expires: expires}
	state.leases++
	return &servingProofLease{state: &servingProofLeaseState{
		attestor: state, challenge: challenge, binding: binding, epoch: state.epoch, expires: expires,
	}}, nil
}

func (l *servingProofLease) Challenge() string {
	if l == nil || l.state == nil {
		return ""
	}
	return l.state.challenge
}

// VerifyResponse authenticates the exact bytes and the TCP connection observed
// by the direct client. clientLocal/clientRemote are the client's connection
// tuple; the handler observes the tuple in reverse.
func (l *servingProofLease) VerifyResponse(body []byte, encodedProof, clientLocal, clientRemote string) error {
	if l == nil || l.state == nil || l.state.attestor == nil || l.state.challenge == "" {
		return ErrServingProofInvalid
	}
	local, err := canonicalServingTCP4Address(clientRemote)
	if err != nil {
		return ErrServingProofInvalid
	}
	remote, err := canonicalServingTCP4Address(clientLocal)
	if err != nil {
		return ErrServingProofInvalid
	}
	proof, err := decodeServingProof(encodedProof)
	if err != nil {
		return ErrServingProofInvalid
	}
	lease := l.state
	state := lease.attestor
	state.mu.Lock()
	defer state.mu.Unlock()
	if lease.released || lease.verified || !state.active || state.aborted || state.epoch != lease.epoch ||
		state.now == nil || !state.now().Before(lease.expires) {
		return ErrServingProofInvalid
	}
	expected := servingResponseMAC(state.secret, lease.challenge, lease.binding, lease.epoch, local, remote, body)
	if subtle.ConstantTimeCompare(proof, expected[:]) != 1 {
		return ErrServingProofInvalid
	}
	lease.verified = true
	return nil
}

// Seal linearises a successfully verified proof into a finalise operation.
// An unexpected listener failure either revokes an unsealed proof first or
// waits for a sealed operation to release its lease.
func (l *servingProofLease) Seal() error {
	if l == nil || l.state == nil || l.state.attestor == nil {
		return ErrServingProofInvalid
	}
	lease := l.state
	state := lease.attestor
	state.mu.Lock()
	defer state.mu.Unlock()
	if lease.released || lease.sealed || !lease.verified || !state.active || state.aborted || state.epoch != lease.epoch ||
		state.now == nil || !state.now().Before(lease.expires) {
		return ErrServingProofInvalid
	}
	lease.sealed = true
	state.sealedLeases++
	return nil
}

// Release is idempotent.
func (l *servingProofLease) Release() {
	if l == nil || l.state == nil {
		return
	}
	lease := l.state
	lease.once.Do(func() {
		if lease.attestor != nil {
			lease.attestor.release(lease)
		}
	})
}

func (a *servingAttestorState) release(lease *servingProofLeaseState) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if lease.released {
		return
	}
	lease.released = true
	delete(a.pending, lease.challenge)
	a.leases--
	if lease.sealed {
		a.sealedLeases--
	}
	if a.aborted {
		if a.sealedLeases == 0 {
			a.finishAbortLocked()
		}
		return
	}
	if a.closing && a.leases == 0 {
		a.invalidateLocked()
	}
}

// BeginClose synchronously rejects new acquisitions and returns a channel that
// closes after all acquired leases release and the proof epoch is invalidated.
func (a *ServingAttestor) BeginClose() <-chan struct{} {
	if a == nil || a.state == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	state := a.state
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.aborted {
		return state.abortDone
	}
	if state.closeDone == nil {
		state.closeDone = make(chan struct{})
	}
	state.closing = true
	if state.leases == 0 {
		state.invalidateLocked()
	}
	return state.closeDone
}

// abortUnexpected revokes proof authority before an unexpected listener error
// is surfaced to http.Server. Sealed operations define the irreversible
// linearisation boundary and are allowed to finish; unsealed leases never hold
// an abnormal server exit open.
func (a *ServingAttestor) abortUnexpected() <-chan struct{} {
	if a == nil || a.state == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	state := a.state
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closing && !state.aborted && state.closeDoneSet {
		return state.closeDone
	}
	if state.abortDone == nil {
		state.abortDone = make(chan struct{})
	}
	if state.closeDone == nil {
		state.closeDone = make(chan struct{})
	}
	if !state.aborted {
		state.closing = true
		state.aborted = true
		state.active = false
		state.listenerAddr = ""
		clear(state.secret[:])
		clear(state.epoch[:])
		clear(state.pending)
	}
	if state.sealedLeases == 0 {
		state.finishAbortLocked()
	}
	return state.abortDone
}

func (a *servingAttestorState) finishAbortLocked() {
	if !a.abortDoneSet {
		close(a.abortDone)
		a.abortDoneSet = true
	}
	if !a.closeDoneSet {
		close(a.closeDone)
		a.closeDoneSet = true
	}
}

func (a *servingAttestorState) invalidateLocked() {
	a.active = false
	a.listenerAddr = ""
	clear(a.secret[:])
	clear(a.epoch[:])
	clear(a.pending)
	if !a.closeDoneSet {
		close(a.closeDone)
		a.closeDoneSet = true
	}
}

// ProveHealth consumes an issued challenge and signs the exact response bytes
// and connection tuple observed by net/http.
func (a *ServingAttestor) ProveHealth(request *http.Request, body []byte) (string, bool) {
	if a == nil || a.state == nil || request == nil || request.Context().Err() != nil {
		return "", false
	}
	values := request.Header.Values(ServingProofChallengeHeader)
	if len(values) != 1 {
		return "", false
	}
	challenge, err := canonicalServingChallenge(values[0])
	if err != nil {
		return "", false
	}
	localAddress, ok := request.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok || localAddress == nil {
		return "", false
	}
	local, err := canonicalServingTCP4Address(localAddress.String())
	if err != nil {
		return "", false
	}
	remote, err := canonicalServingTCP4Address(request.RemoteAddr)
	if err != nil {
		return "", false
	}
	state := a.state
	state.mu.Lock()
	defer state.mu.Unlock()
	pending, exists := state.pending[challenge]
	if exists {
		delete(state.pending, challenge)
	}
	if !exists || !state.active || local != state.listenerAddr || state.now == nil || !state.now().Before(pending.expires) || pending.epoch != state.epoch {
		return "", false
	}
	proof := servingResponseMAC(state.secret, challenge, pending.binding, pending.epoch, local, remote, body)
	return base64.RawURLEncoding.EncodeToString(proof[:]), true
}

func canonicalServingChallenge(value string) (string, error) {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || len(decoded) != servingAttestorNonceSize || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return "", ErrServingProofInvalid
	}
	return value, nil
}

func decodeServingProof(value string) ([]byte, error) {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || len(decoded) != sha256.Size || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return nil, ErrServingProofInvalid
	}
	return decoded, nil
}

func canonicalServingTCP4Address(value string) (string, error) {
	address, err := netip.ParseAddrPort(value)
	if err != nil || !address.Addr().Is4() || !address.Addr().IsLoopback() || address.Port() == 0 {
		return "", ErrServingProofInvalid
	}
	return address.String(), nil
}

func servingResponseMAC(secret [servingAttestorSecretSize]byte, challenge string, binding [sha256.Size]byte, epoch [servingAttestorEpochSize]byte, local, remote string, body []byte) [sha256.Size]byte {
	mac := hmac.New(sha256.New, secret[:])
	writeServingProofField(mac, []byte("cq-serving-health-response-v1"))
	writeServingProofField(mac, []byte(challenge))
	writeServingProofField(mac, binding[:])
	writeServingProofField(mac, epoch[:])
	writeServingProofField(mac, []byte(local))
	writeServingProofField(mac, []byte(remote))
	bodyDigest := sha256.Sum256(body)
	writeServingProofField(mac, bodyDigest[:])
	var proof [sha256.Size]byte
	copy(proof[:], mac.Sum(nil))
	return proof
}

func writeServingProofField(destination hash.Hash, value []byte) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	_, _ = destination.Write(length[:])
	_, _ = destination.Write(value)
}
