package proxy

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

var ErrRuntimeUpgradeUnsupported = errors.New("runtime does not support reversible upgrade")

var ErrRuntimeUpgradePaused = errors.New("runtime upgrade admissions paused")

type RuntimeUpgradeWorker interface {
	PrepareUpgrade(context.Context) error
	AwaitUpgradeQuiescence(context.Context) error
	ResumeUpgrade(context.Context) error
}

type RuntimeUpgradeAdmission struct {
	mu           sync.Mutex
	active       int
	paused       bool
	zero         chan struct{}
	pause        chan struct{}
	resume       chan struct{}
	sessions     int
	sessionsZero chan struct{}
}

func NewRuntimeUpgradeAdmission() *RuntimeUpgradeAdmission {
	zero := make(chan struct{})
	close(zero)
	resume := make(chan struct{})
	close(resume)
	return &RuntimeUpgradeAdmission{zero: zero, sessionsZero: zero, pause: make(chan struct{}), resume: resume}
}
func (gate *RuntimeUpgradeAdmission) begin(accepted bool) (func(), error) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.paused && !accepted {
		return nil, ErrRuntimeUpgradePaused
	}
	if gate.active == 0 {
		gate.zero = make(chan struct{})
	}
	gate.active++
	var once sync.Once
	return func() {
		once.Do(func() {
			gate.mu.Lock()
			defer gate.mu.Unlock()
			gate.active--
			if gate.active == 0 {
				close(gate.zero)
			}
		})
	}, nil
}
func (gate *RuntimeUpgradeAdmission) BeginRequest() (func(), error) { return gate.begin(false) }
func (gate *RuntimeUpgradeAdmission) BeginTurn() (func(), error)    { return gate.begin(false) }
func (gate *RuntimeUpgradeAdmission) Pause(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if !gate.paused {
		gate.paused = true
		gate.resume = make(chan struct{})
		close(gate.pause)
	}
	return nil
}
func (gate *RuntimeUpgradeAdmission) Resume() {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.paused {
		gate.paused = false
		gate.pause = make(chan struct{})
		close(gate.resume)
	}
}
func (gate *RuntimeUpgradeAdmission) PauseSignal() <-chan struct{} {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return gate.pause
}
func (gate *RuntimeUpgradeAdmission) AwaitQuiescence(ctx context.Context) error {
	gate.mu.Lock()
	zero := gate.zero
	sessionsZero := gate.sessionsZero
	paused := gate.paused
	gate.mu.Unlock()
	select {
	case <-zero:
	case <-ctx.Done():
		return ctx.Err()
	}
	if paused {
		select {
		case <-sessionsZero:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
func (gate *RuntimeUpgradeAdmission) beginSession() (func(), error) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.paused {
		return nil, ErrRuntimeUpgradePaused
	}
	if gate.sessions == 0 {
		gate.sessionsZero = make(chan struct{})
	}
	gate.sessions++
	var once sync.Once
	return func() {
		once.Do(func() {
			gate.mu.Lock()
			defer gate.mu.Unlock()
			gate.sessions--
			if gate.sessions == 0 {
				close(gate.sessionsZero)
			}
		})
	}, nil
}

func (gate *RuntimeUpgradeAdmission) quiescent() bool {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return gate.active == 0 && (!gate.paused || gate.sessions == 0)
}

type runtimeUpgradeRequestReleaseKey struct{}

func RuntimeUpgradeHTTPHandler(handler http.Handler, gate *RuntimeUpgradeAdmission) http.Handler {
	return runtimeUpgradeHTTPHandler(handler, gate, false)
}
func runtimeUpgradeHTTPHandler(handler http.Handler, gate *RuntimeUpgradeAdmission, releaseOnHijack bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every connection reaching the private worker was accepted by its supervisor.
		release, _ := gate.begin(true)
		defer release()
		writer := &runtimeUpgradeResponseWriter{ResponseWriter: w, release: release, releaseOnHijack: releaseOnHijack}
		handler.ServeHTTP(writer, r.WithContext(context.WithValue(r.Context(), runtimeUpgradeRequestReleaseKey{}, release)))
	})
}

type runtimeUpgradeResponseWriter struct {
	http.ResponseWriter
	release         func()
	releaseOnHijack bool
}

func (writer *runtimeUpgradeResponseWriter) Unwrap() http.ResponseWriter {
	return writer.ResponseWriter
}
func (writer *runtimeUpgradeResponseWriter) Flush() {
	_ = http.NewResponseController(writer.ResponseWriter).Flush()
}
func (writer *runtimeUpgradeResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(writer.ResponseWriter).Hijack()
	if err == nil && writer.releaseOnHijack {
		writer.release()
	}
	return conn, rw, err
}

// RuntimeUpgradeListener pauses Accept without closing the kernel TCP socket.
// Connections arriving during handoff remain bounded by the kernel backlog.
type RuntimeUpgradeListener struct {
	tcp         *net.TCPListener
	acceptTCP   func() (net.Conn, error)
	mu          sync.Mutex
	paused      bool
	closed      bool
	accepting   bool
	changed     chan struct{}
	connections *RuntimeUpgradeConnections
}

func NewRuntimeUpgradeListener(tcp *net.TCPListener) *RuntimeUpgradeListener {
	return &RuntimeUpgradeListener{tcp: tcp, acceptTCP: tcp.Accept, changed: make(chan struct{})}
}
func (listener *RuntimeUpgradeListener) notify() {
	close(listener.changed)
	listener.changed = make(chan struct{})
}
func (listener *RuntimeUpgradeListener) Addr() net.Addr          { return listener.tcp.Addr() }
func (listener *RuntimeUpgradeListener) File() (*os.File, error) { return listener.tcp.File() }
func (listener *RuntimeUpgradeListener) Close() error {
	listener.mu.Lock()
	listener.closed = true
	listener.notify()
	listener.mu.Unlock()
	return listener.tcp.Close()
}
func (listener *RuntimeUpgradeListener) Accept() (net.Conn, error) {
	for {
		listener.mu.Lock()
		if listener.closed {
			listener.mu.Unlock()
			return nil, net.ErrClosed
		}
		if listener.paused {
			changed := listener.changed
			listener.mu.Unlock()
			<-changed
			continue
		}
		listener.accepting = true
		listener.mu.Unlock()
		conn, err := listener.acceptTCP()
		listener.mu.Lock()
		listener.accepting = false
		listener.notify()
		paused, closed := listener.paused, listener.closed
		if conn != nil {
			// A successful kernel accept cannot be returned to the backlog. Track
			// and deliver it before Pause can acknowledge, then drain it normally.
			if listener.closed {
				listener.mu.Unlock()
				conn.Close()
				return nil, net.ErrClosed
			}
			if listener.connections != nil {
				conn = listener.connections.track(conn)
			}
			listener.mu.Unlock()
			return conn, nil
		}
		listener.mu.Unlock()
		if closed {
			return nil, net.ErrClosed
		}
		if paused {
			continue
		}
		return nil, err
	}
}
func (listener *RuntimeUpgradeListener) Pause(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	listener.mu.Lock()
	listener.paused = true
	listener.notify()
	_ = listener.tcp.SetDeadline(time.Now())
	for listener.accepting {
		changed := listener.changed
		listener.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			listener.Resume()
			return ctx.Err()
		}
		listener.mu.Lock()
	}
	listener.mu.Unlock()
	return nil
}
func (listener *RuntimeUpgradeListener) Resume() {
	listener.mu.Lock()
	defer listener.mu.Unlock()
	listener.paused = false
	_ = listener.tcp.SetDeadline(time.Time{})
	listener.notify()
}

type RuntimeUpgradeConnections struct {
	Admission *RuntimeUpgradeAdmission
	mu        sync.Mutex
	releases  map[net.Conn]func()
}

func NewRuntimeUpgradeConnections() *RuntimeUpgradeConnections {
	return &RuntimeUpgradeConnections{Admission: NewRuntimeUpgradeAdmission(), releases: make(map[net.Conn]func())}
}
func (connections *RuntimeUpgradeConnections) track(conn net.Conn) net.Conn {
	wrapped := &runtimeUpgradeConn{Conn: conn, connections: connections}
	connections.ConnState(wrapped, http.StateNew)
	return wrapped
}
func (connections *RuntimeUpgradeConnections) ConnState(conn net.Conn, state http.ConnState) {
	connections.mu.Lock()
	defer connections.mu.Unlock()
	switch state {
	case http.StateNew, http.StateActive:
		if connections.releases[conn] == nil {
			release, _ := connections.Admission.begin(true)
			connections.releases[conn] = release
		}
	case http.StateIdle, http.StateHijacked, http.StateClosed:
		if release := connections.releases[conn]; release != nil {
			release()
			delete(connections.releases, conn)
		}
	}
}

type runtimeUpgradeConn struct {
	net.Conn
	connections *RuntimeUpgradeConnections
}

func (conn *runtimeUpgradeConn) Close() error {
	err := conn.Conn.Close()
	conn.connections.ConnState(conn, http.StateClosed)
	return err
}
func (listener *RuntimeUpgradeListener) SetConnectionTracker(connections *RuntimeUpgradeConnections) {
	listener.mu.Lock()
	defer listener.mu.Unlock()
	listener.connections = connections
}
