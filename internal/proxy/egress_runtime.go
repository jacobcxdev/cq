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
	Socket string `json:"socket"`
	Token  string `json:"token"`
}

func runtimeEgressDescriptorPath(dir string, port int) string {
	return filepath.Join(dir, "egress-"+strconv.Itoa(port)+".json")
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
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		listener.Close()
		cancel()
		manager.Close()
		cleanupSocket()
		return nil, errors.New("create egress authority failed")
	}
	token := base64.RawURLEncoding.EncodeToString(random[:])
	clear(random[:])
	data, _ := json.Marshal(runtimeEgressDescriptor{Socket: socketPath, Token: token})
	descriptorPath := runtimeEgressDescriptorPath(dir, cfg.Port)
	if err := fsutil.SecureAtomicWrite(fs, descriptorPath, data); err != nil {
		listener.Close()
		cancel()
		manager.Close()
		cleanupSocket()
		return nil, errors.New("publish private egress descriptor failed")
	}
	// Remember the exact published inode so shutdown never removes a successor's
	// descriptor, even if another owner replaced it in the meantime.
	var identity fsutil.SecureFileIdentity
	inspector, inspectOK := fs.(fsutil.SecurePathInspector)
	identityOK := false
	if inspectOK {
		if info, e := inspector.Lstat(descriptorPath); e == nil {
			identity, identityOK = inspector.FileIdentity(info)
		}
	}
	tracked := &runtimeEgressListener{Listener: listener, connections: make(map[*runtimeEgressConn]struct{})}
	handler := NewEgressBroker(manager, token, manager.AllowsTarget)
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
		handler.ServeHTTP(w, r)
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
		if identityOK {
			if opener, ok := fs.(fsutil.SecureDirectoryOpener); ok {
				if directory, openErr := opener.OpenSecureDirectory(dir); openErr == nil {
					if remover, ok := directory.(fsutil.IdentityBoundRemover); ok {
						_ = remover.RemoveChecked(filepath.Base(descriptorPath), identity)
					}
					_ = directory.Close()
				}
			}
		}
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
	invalid := errors.New("runtime egress descriptor unavailable or invalid; no direct fallback")
	if port < 1 || port > 65535 || !fsutil.IsCleanAbsoluteNonRootPath(dir) {
		return nil, invalid
	}
	data, err := fsutil.ReadSecureFile(fs, runtimeEgressDescriptorPath(dir, port), 8192)
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
	raw, err := base64.RawURLEncoding.DecodeString(descriptor.Token)
	if err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != descriptor.Token {
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
	client := NewEgressBrokerClient(descriptor.Socket, descriptor.Token)
	return codexEgressWithDialer(client.DialContext, nil)
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
