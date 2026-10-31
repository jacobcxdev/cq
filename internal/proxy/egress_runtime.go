package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

type runtimeEgressManager interface {
	EgressDialer
	AllowsTarget(string) bool
	Reload(*Config) error
	Block()
	Close() error
}
type runtimeEgressDependencies struct {
	newManager func(*Config, fsutil.FileSystem) (runtimeEgressManager, error)
	load       func() (*Config, error)
	interval   time.Duration
}

func defaultRuntimeEgressDependencies() runtimeEgressDependencies {
	return runtimeEgressDependencies{
		newManager: func(cfg *Config, fs fsutil.FileSystem) (runtimeEgressManager, error) {
			return NewCodexEgressManager(cfg, fs)
		}, load: LoadCodexEgressConfig, interval: 5 * time.Second,
	}
}

type runtimeEgressDescriptor struct {
	Socket      string `json:"socket"`
	Token       string `json:"token,omitempty"`
	NativeToken string `json:"native_token,omitempty"`
}

func runtimeEgressDescriptorPath(dir string, port int) string {
	return filepath.Join(dir, "egress-"+strconv.Itoa(port)+".json")
}

func nativeEgressDescriptorPath(dir string, port int) string {
	return filepath.Join(dir, "native-egress-"+strconv.Itoa(port)+".json")
}

// StartRuntimeEgress owns the manager and private broker for one supervisor.
// Workers receive only authenticated stream access, never profile contents.
func StartRuntimeEgress(ctx context.Context, cfg *Config, fs fsutil.FileSystem) (*CodexEgress, error) {
	paths, err := ResolveDefaultPaths()
	if err != nil {
		return nil, errors.New("resolve runtime egress directory failed")
	}
	return startRuntimeEgress(ctx, cfg, fs, paths.RuntimeDir, defaultRuntimeEgressDependencies())
}

func startRuntimeEgress(ctx context.Context, cfg *Config, fs fsutil.FileSystem, dir string, deps runtimeEgressDependencies) (*CodexEgress, error) {
	if cfg == nil || cfg.Port < 1 || cfg.Port > 65535 || !fsutil.IsCleanAbsoluteNonRootPath(dir) {
		return nil, errors.New("invalid runtime egress configuration")
	}
	if err := fsutil.EnsureSecureDirectory(fs, dir); err != nil {
		return nil, errors.New("runtime egress requires a private directory")
	}
	manager, err := deps.newManager(cfg, fs)
	if err != nil {
		return nil, err
	}
	life, cancel := context.WithCancel(ctx)
	socketDir, err := os.MkdirTemp(dir, "e-")
	if err != nil {
		cancel()
		manager.Close()
		return nil, errors.New("create private egress directory failed")
	}
	socketPath := filepath.Join(socketDir, "s")
	cleanupSocket := func() { _ = os.Remove(socketPath); _ = os.Remove(socketDir) }
	if len(socketPath) > 103 {
		cancel()
		manager.Close()
		cleanupSocket()
		return nil, errors.New("runtime egress socket path exceeds platform limit")
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		cancel()
		manager.Close()
		cleanupSocket()
		return nil, errors.New("create private egress listener failed")
	}
	if unixListener, ok := listener.(*net.UnixListener); ok {
		unixListener.SetUnlinkOnClose(false)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		listener.Close()
		cancel()
		manager.Close()
		cleanupSocket()
		return nil, errors.New("secure private egress listener failed")
	}
	var random [64]byte
	if _, err := rand.Read(random[:]); err != nil {
		listener.Close()
		cancel()
		manager.Close()
		cleanupSocket()
		return nil, errors.New("create egress authority failed")
	}
	token := base64.RawURLEncoding.EncodeToString(random[:32])
	nativeToken := base64.RawURLEncoding.EncodeToString(random[32:])
	clear(random[:])
	// Keep the worker schema unchanged: already-running workers reject unknown
	// fields. Native authority has a separate descriptor and capability.
	workerData, _ := json.Marshal(runtimeEgressDescriptor{Socket: socketPath, Token: token})
	nativeData, _ := json.Marshal(runtimeEgressDescriptor{Socket: socketPath, NativeToken: nativeToken})
	defer clear(workerData)
	defer clear(nativeData)
	inspector, inspectOK := fs.(fsutil.SecurePathInspector)
	identities := make(map[string]fsutil.SecureFileIdentity)
	cleanupDescriptors := func() {
		if opener, ok := fs.(fsutil.SecureDirectoryOpener); ok {
			if directory, err := opener.OpenSecureDirectory(dir); err == nil {
				defer directory.Close()
				if remover, ok := directory.(fsutil.IdentityBoundRemover); ok {
					for name, identity := range identities {
						_ = remover.RemoveChecked(name, identity)
					}
				}
			}
		}
	}
	for _, descriptor := range []struct {
		path string
		data []byte
	}{{runtimeEgressDescriptorPath(dir, cfg.Port), workerData}, {nativeEgressDescriptorPath(dir, cfg.Port), nativeData}} {
		if err := fsutil.SecureAtomicWrite(fs, descriptor.path, descriptor.data); err != nil {
			listener.Close()
			cancel()
			manager.Close()
			cleanupDescriptors()
			cleanupSocket()
			return nil, errors.New("publish private egress descriptor failed")
		}
		// Never remove a successor's descriptor on shutdown.
		if inspectOK {
			if info, err := inspector.Lstat(descriptor.path); err == nil {
				if identity, ok := inspector.FileIdentity(info); ok {
					identities[filepath.Base(descriptor.path)] = identity
				}
			}
		}
	}
	tracked := &runtimeEgressListener{Listener: listener, connections: make(map[*runtimeEgressConn]struct{})}
	handler := NewEgressBroker(manager, token, manager.AllowsTarget)
	var nativeDialer EgressDialer
	if native, ok := manager.(interface {
		DialNativeContext(context.Context, string, string) (net.Conn, error)
		NativeReady() bool
	}); ok {
		nativeDialer = nativeRuntimeDialer{native.DialNativeContext, native.NativeReady}
	}
	nativeHandler := NewNativeEgressBroker(nativeDialer, nativeToken)
	server := &http.Server{ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 32 * 1024, BaseContext: func(net.Listener) context.Context { return life }, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tracked.mu.Lock()
		if tracked.closed {
			tracked.mu.Unlock()
			http.Error(w, "egress stopped", http.StatusServiceUnavailable)
			return
		}
		tracked.handlers.Add(1)
		tracked.mu.Unlock()
		defer tracked.handlers.Done()
		if r.Header.Get("X-CQ-Native-Flow") == "1" {
			nativeHandler.ServeHTTP(w, r)
		} else {
			handler.ServeHTTP(w, r)
		}
	})}
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		defer func() {
			if recover() != nil {
				manager.Block()
			}
		}()
		if err := server.Serve(tracked); err != nil && !errors.Is(err, http.ErrServerClosed) {
			manager.Block()
		}
	}()
	pollDone := startRuntimeEgressPoller(life, manager, deps)
	var stopParent func() bool
	var stopMu sync.Mutex
	closeOwner := func() error {
		stopMu.Lock()
		if stopParent != nil {
			stopParent()
		}
		stopMu.Unlock()
		cancel()
		tracked.mu.Lock()
		tracked.closed = true
		tracked.mu.Unlock()
		_ = server.Close()
		_ = listener.Close()
		tracked.mu.Lock()
		connections := make([]*runtimeEgressConn, 0, len(tracked.connections))
		for c := range tracked.connections {
			connections = append(connections, c)
		}
		tracked.mu.Unlock()
		for _, c := range connections {
			_ = c.Close()
		}
		<-serveDone
		<-pollDone
		tracked.handlers.Wait()
		err := manager.Close()
		cleanupDescriptors()
		cleanupSocket()
		return err
	}
	egress, err := codexEgressWithDialer(manager.DialContext, closeOwner)
	if err != nil {
		_ = closeOwner()
		return nil, err
	}
	stopMu.Lock()
	stopParent = context.AfterFunc(ctx, func() { defer func() { _ = recover() }(); _ = egress.Close() })
	stopMu.Unlock()
	return egress, nil
}

func startRuntimeEgressPoller(ctx context.Context, manager runtimeEgressManager, deps runtimeEgressDependencies) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if recover() != nil {
				manager.Block()
			}
		}()
		ticker := time.NewTicker(deps.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cfg, err := deps.load()
				if err != nil || cfg == nil {
					manager.Block()
					continue
				}
				if err := manager.Reload(cfg); err != nil {
					manager.Block()
				}
			}
		}
	}()
	return done
}

// OpenReloadingCodexEgress is the foreground owner without a broker process.
func OpenReloadingCodexEgress(ctx context.Context, cfg *Config, fs fsutil.FileSystem) (*CodexEgress, error) {
	return openReloadingCodexEgress(ctx, cfg, fs, defaultRuntimeEgressDependencies())
}
func openReloadingCodexEgress(ctx context.Context, cfg *Config, fs fsutil.FileSystem, deps runtimeEgressDependencies) (*CodexEgress, error) {
	manager, err := deps.newManager(cfg, fs)
	if err != nil {
		return nil, err
	}
	life, cancel := context.WithCancel(ctx)
	done := startRuntimeEgressPoller(life, manager, deps)
	var stopParent func() bool
	var stopMu sync.Mutex
	egress, err := codexEgressWithDialer(manager.DialContext, func() error {
		stopMu.Lock()
		if stopParent != nil {
			stopParent()
		}
		stopMu.Unlock()
		cancel()
		<-done
		return manager.Close()
	})
	if err != nil {
		cancel()
		<-done
		manager.Close()
		return nil, err
	}
	stopMu.Lock()
	stopParent = context.AfterFunc(ctx, func() { defer func() { _ = recover() }(); _ = egress.Close() })
	stopMu.Unlock()
	return egress, nil
}

// OpenRuntimeEgressClient reads only a bounded private broker descriptor. Socket
// reachability and authority are checked on every actual connection attempt.
func OpenRuntimeEgressClient(port int, fs fsutil.FileSystem) (*CodexEgress, error) {
	paths, err := ResolveDefaultPaths()
	if err != nil {
		return nil, errors.New("resolve runtime egress directory failed")
	}
	return openRuntimeEgressClient(port, fs, paths.RuntimeDir)
}
func openRuntimeEgressClient(port int, fs fsutil.FileSystem, dir string) (*CodexEgress, error) {
	descriptor, err := readRuntimeEgressDescriptor(port, fs, dir)
	if err != nil {
		return nil, err
	}
	client := NewEgressBrokerClient(descriptor.Socket, descriptor.Token)
	return codexEgressWithDialer(client.DialContext, nil)
}

func readRuntimeEgressDescriptor(port int, fs fsutil.FileSystem, dir string) (*runtimeEgressDescriptor, error) {
	return readEgressDescriptor(fs, runtimeEgressDescriptorPath(dir, port), dir, port, false)
}

func readEgressDescriptor(fs fsutil.FileSystem, path, dir string, port int, native bool) (*runtimeEgressDescriptor, error) {
	invalid := errors.New("runtime egress descriptor unavailable or invalid; no direct fallback")
	if port < 1 || port > 65535 || !fsutil.IsCleanAbsoluteNonRootPath(dir) {
		return nil, invalid
	}
	data, err := fsutil.ReadSecureFile(fs, path, 8192)
	if err != nil {
		return nil, invalid
	}
	defer clear(data)
	var descriptor runtimeEgressDescriptor
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&descriptor); err != nil {
		return nil, invalid
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, invalid
	}
	token := descriptor.Token
	if native {
		token = descriptor.NativeToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != token {
		return nil, invalid
	}
	clear(raw)
	if !fsutil.IsCleanAbsoluteNonRootPath(descriptor.Socket) || len(descriptor.Socket) > 103 {
		return nil, invalid
	}
	socketDir := filepath.Dir(descriptor.Socket)
	if filepath.Dir(socketDir) != dir || !strings.HasPrefix(filepath.Base(socketDir), "e-") || filepath.Base(descriptor.Socket) != "s" {
		return nil, invalid
	}
	if err := fsutil.ValidateSecureDirectory(fs, socketDir); err != nil {
		return nil, invalid
	}
	inspector, ok := fs.(fsutil.SecurePathInspector)
	if !ok {
		return nil, invalid
	}
	info, err := inspector.Lstat(descriptor.Socket)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		return nil, invalid
	}
	if err := fsutil.ValidateSecureOwner(inspector, info); err != nil {
		return nil, invalid
	}
	return &descriptor, nil
}

type nativeRuntimeDialer struct {
	dial  func(context.Context, string, string) (net.Conn, error)
	ready func() bool
}

func (d nativeRuntimeDialer) DialContext(ctx context.Context, network, target string) (net.Conn, error) {
	return d.dial(ctx, network, target)
}

func (d nativeRuntimeDialer) NativeReady() bool { return d.ready != nil && d.ready() }

// NativeEgressDescriptorPath exposes only a validated path to the user-context
// companion. Broker capabilities stay in the existing private descriptor.
func NativeEgressDescriptorPath(fs fsutil.FileSystem) (string, error) {
	cfg, err := LoadExistingConfig()
	if err != nil || cfg == nil || cfg.CodexWireGuardProfile == "" {
		return "", errors.New("native egress requires configured WireGuard")
	}
	paths, err := ResolveDefaultPaths()
	if err != nil {
		return "", errors.New("native egress runtime directory unavailable")
	}
	descriptor, err := readEgressDescriptor(fs, nativeEgressDescriptorPath(paths.RuntimeDir, cfg.Port), paths.RuntimeDir, cfg.Port, true)
	if err != nil || descriptor.NativeToken == "" {
		return "", errors.New("native egress broker unavailable; update the CQ service")
	}
	return nativeEgressDescriptorPath(paths.RuntimeDir, cfg.Port), nil
}

type runtimeEgressListener struct {
	net.Listener
	mu          sync.Mutex
	closed      bool
	connections map[*runtimeEgressConn]struct{}
	handlers    sync.WaitGroup
}
type runtimeEgressConn struct {
	net.Conn
	owner *runtimeEgressListener
	once  sync.Once
}

func (l *runtimeEgressListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		c.Close()
		return nil, net.ErrClosed
	}
	wrapped := &runtimeEgressConn{Conn: c, owner: l}
	l.connections[wrapped] = struct{}{}
	return wrapped, nil
}
func (c *runtimeEgressConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.owner.mu.Lock(); delete(c.owner.connections, c); c.owner.mu.Unlock() })
	return err
}
