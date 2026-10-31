package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strings"
	"sync"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

type egressGeneration struct {
	backend *CodexEgress
	life    context.Context
	cancel  context.CancelFunc
	digest  [32]byte
}
type egressOpener func(*Config) (*CodexEgress, [32]byte, error)

// EgressManager owns the only active VPN generation for an instance. Replacing
// a profile closes old tunnels before exposing the new generation. Existing
// streams reconnect; the service and worker processes do not need a restart.
type EgressManager struct {
	change  sync.Mutex
	mu      sync.Mutex
	current *egressGeneration
	cfg     Config
	open    egressOpener
	active  map[*managedEgressConn]struct{}
	closed  bool
	blocked bool
}

func NewCodexEgressManager(cfg *Config, fs fsutil.FileSystem) (*EgressManager, error) {
	return newEgressManager(cfg, func(cfg *Config) (*CodexEgress, [32]byte, error) {
		var zero [32]byte
		// The fingerprint stays private and is never used as a credential or logged.
		metadata, _ := json.Marshal(struct{ Proxy, Profile, Interface, Origin string }{cfg.CodexEgressProxy, cfg.CodexWireGuardProfile, cfg.CodexEgressInterface, cfg.CodexUpstream})
		hash := sha256.New()
		hash.Write(metadata)
		if cfg.CodexWireGuardProfile != "" {
			if err := ValidateCodexEgressSettings(cfg.CodexEgressProxy, cfg.CodexWireGuardProfile); err != nil {
				return nil, zero, err
			}
			if err := validateCodexEgressInterface(cfg.CodexWireGuardProfile, cfg.CodexEgressInterface); err != nil {
				return nil, zero, err
			}
			data, err := fsutil.ReadSecureFile(fs, cfg.CodexWireGuardProfile, 64*1024)
			if err != nil {
				return nil, zero, errors.New("read private WireGuard reload profile failed")
			}
			hash.Write(data)
			profile, parseErr := parseWireGuardProfile(data)
			clear(data)
			if parseErr != nil {
				return nil, zero, parseErr
			}
			backend, err := openCodexWireGuardEgress(cfg, profile)
			if err != nil {
				return nil, zero, err
			}
			var digest [32]byte
			copy(digest[:], hash.Sum(nil))
			return backend, digest, nil
		}
		backend, err := OpenCodexEgress(cfg, fs)
		if err != nil {
			return nil, zero, err
		}
		var digest [32]byte
		copy(digest[:], hash.Sum(nil))
		return backend, digest, nil
	})
}

func newEgressManager(cfg *Config, open egressOpener) (*EgressManager, error) {
	if cfg == nil || open == nil {
		return nil, errors.New("Codex egress manager configuration unavailable")
	}
	backend, digest, err := open(cfg)
	if err != nil {
		return nil, errors.New("initialize Codex egress manager failed")
	}
	life, cancel := context.WithCancel(context.Background())
	return &EgressManager{current: &egressGeneration{backend: backend, digest: digest, life: life, cancel: cancel}, cfg: *cfg, open: open, active: make(map[*managedEgressConn]struct{})}, nil
}

func (m *EgressManager) DialContext(parent context.Context, network, address string) (net.Conn, error) {
	return m.dialContext(parent, network, address, false)
}

// DialNativeContext is reserved for authenticated native-app flows. It cannot
// select ambient or direct networking when the WireGuard profile is absent.
func (m *EgressManager) DialNativeContext(parent context.Context, network, address string) (net.Conn, error) {
	if !nativeEgressTarget(address) {
		return nil, errors.New("native egress destination denied")
	}
	return m.dialContext(parent, network, address, true)
}

// NativeReady checks transport ownership without opening an origin connection.
func (m *EgressManager) NativeReady() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.closed && !m.blocked && m.current != nil && m.cfg.CodexWireGuardProfile != "" && m.current.backend.nativeDial != nil
}

func (m *EgressManager) dialContext(parent context.Context, network, address string, native bool) (net.Conn, error) {
	if err := parent.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.closed || m.blocked || m.current == nil {
		m.mu.Unlock()
		return nil, errors.New("Codex egress is unavailable; traffic is blocked")
	}
	generation := m.current
	dial := generation.backend.DialContext
	if native {
		if m.cfg.CodexWireGuardProfile == "" || generation.backend.nativeDial == nil {
			m.mu.Unlock()
			return nil, errors.New("native egress requires WireGuard; traffic is blocked")
		}
		dial = generation.backend.nativeDial
	}
	m.mu.Unlock()
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(generation.life, cancel)
	connection, err := dial(ctx, network, address)
	stop()
	cancel()
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.closed || m.blocked || m.current != generation || generation.life.Err() != nil {
		m.mu.Unlock()
		connection.Close()
		return nil, errors.New("Codex egress changed during connection")
	}
	managed := &managedEgressConn{Conn: connection, owner: m}
	m.active[managed] = struct{}{}
	m.mu.Unlock()
	return managed, nil
}

func (m *EgressManager) Reload(cfg *Config) error {
	m.change.Lock()
	defer m.change.Unlock()
	if cfg == nil {
		m.blockLocked()
		return errors.New("Codex egress reload failed; traffic is blocked")
	}
	candidate, digest, err := m.open(cfg)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		if candidate != nil {
			candidate.Close()
		}
		return net.ErrClosed
	}
	if err == nil && !m.blocked && m.current != nil && m.current.digest == digest {
		m.cfg = *cfg
		m.mu.Unlock()
		candidate.Close()
		return nil
	}
	m.mu.Unlock()
	m.blockLocked()
	if err != nil {
		if candidate != nil {
			candidate.Close()
		}
		return errors.New("Codex egress reload failed; traffic is blocked")
	}
	life, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.current = &egressGeneration{backend: candidate, digest: digest, life: life, cancel: cancel}
	m.cfg = *cfg
	m.blocked = false
	m.mu.Unlock()
	return nil
}

// Block is used when configuration itself cannot be read/validated. It closes
// existing tunnels too, so connection pooling cannot bypass a failed reload.
func (m *EgressManager) Block() { m.change.Lock(); defer m.change.Unlock(); m.blockLocked() }
func (m *EgressManager) blockLocked() {
	m.mu.Lock()
	m.blocked = true
	old := m.current
	m.current = nil
	connections := make([]*managedEgressConn, 0, len(m.active))
	for c := range m.active {
		connections = append(connections, c)
	}
	m.mu.Unlock()
	if old != nil {
		old.cancel()
	}
	for _, c := range connections {
		c.Close()
	}
	if old != nil {
		old.backend.Close()
	}
}
func (m *EgressManager) Close() error {
	m.change.Lock()
	defer m.change.Unlock()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.mu.Unlock()
	m.blockLocked()
	return nil
}

func (m *EgressManager) AllowsTarget(target string) bool {
	if !canonicalEgressBrokerTarget(target) {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.blocked || m.current == nil {
		return false
	}
	origin := m.cfg.CodexUpstream
	if origin == "" {
		origin = DefaultCodexUpstream
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "http" {
			port = "80"
		} else {
			port = "443"
		}
	}
	return target == net.JoinHostPort(strings.ToLower(u.Hostname()), port) || IsCodexServiceTarget(target)
}

// IsCodexServiceTarget identifies the public control-plane origins shared by
// Codex and Dots. It does not permit arbitrary desktop or browser traffic.
func IsCodexServiceTarget(target string) bool {
	switch target {
	case "chatgpt.com:443", "auth.openai.com:443", "api.openai.com:443",
		"ws.chatgpt.com:443", "realtime.chatgpt.com:443",
		"codex-cloud-backend.chatgpt.com:443", "codex-cloud-environments.chatgpt.com:443":
		return true
	}
	return false
}

type managedEgressConn struct {
	net.Conn
	owner *EgressManager
	once  sync.Once
	err   error
}

func (c *managedEgressConn) Close() error {
	c.once.Do(func() { c.err = c.Conn.Close(); c.owner.mu.Lock(); delete(c.owner.active, c); c.owner.mu.Unlock() })
	return c.err
}
