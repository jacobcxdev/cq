package dots

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
)

type dotsNetwork struct {
	load   func() (*proxy.Config, error)
	broker func(int) (proxy.EgressDialer, error)
	direct func(context.Context, string, string) (net.Conn, error)
}

// dotsDialContext shares the supervisor's existing egress instance. Dots never
// read a WireGuard key or start a second device, and never fall back from a
// configured tunnel to the host route. Local IPC and unrelated desktop hosts
// retain their existing route.
func dotsDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	n := dotsNetwork{
		load: proxy.LoadExistingConfig,
		broker: func(port int) (proxy.EgressDialer, error) {
			return proxy.OpenRuntimeEgressClient(port, fsutil.OSFileSystem{})
		},
		direct: (&net.Dialer{Timeout: 25 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	}
	return n.DialContext(ctx, network, address)
}

func (n dotsNetwork) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !proxy.IsCodexServiceTarget(address) {
		return n.direct(ctx, network, address)
	}
	cfg, err := n.load()
	if err != nil || cfg == nil {
		return nil, errors.New("Dots egress configuration unavailable; traffic is blocked")
	}
	if cfg.CodexWireGuardProfile == "" && (cfg.CodexEgressProxy == "" || cfg.CodexEgressProxy == "direct") {
		return n.direct(ctx, network, address)
	}
	// Reload the private descriptor for each connection, so a supervisor restart
	// cannot strand this process on a stale socket or broker credential.
	broker, err := n.broker(cfg.Port)
	if err != nil || broker == nil {
		return nil, errors.New("Dots egress broker unavailable; no direct fallback")
	}
	conn, err := broker.DialContext(ctx, network, address)
	if err != nil {
		return nil, errors.New("Dots egress connection failed; no direct fallback")
	}
	return conn, nil
}
