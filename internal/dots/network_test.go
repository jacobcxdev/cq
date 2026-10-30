package dots

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/proxy"
)

type dotsTestDial func(context.Context, string, string) (net.Conn, error)

func (f dotsTestDial) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return f(ctx, network, address)
}

func TestDotsNetworkUsesSharedEgressForControlPlane(t *testing.T) {
	for _, address := range []string{
		"chatgpt.com:443", "auth.openai.com:443", "api.openai.com:443",
		"ws.chatgpt.com:443", "realtime.chatgpt.com:443",
		"codex-cloud-backend.chatgpt.com:443", "codex-cloud-environments.chatgpt.com:443",
	} {
		t.Run(address, func(t *testing.T) {
			for _, cfg := range []*proxy.Config{
				{Port: 23456, CodexWireGuardProfile: "/private/us.conf"},
				{Port: 23456, CodexEgressProxy: "socks5h://127.0.0.1:1080"},
			} {
				calls := 0
				var peer net.Conn
				n := dotsNetwork{
					load: func() (*proxy.Config, error) { return cfg, nil },
					direct: func(context.Context, string, string) (net.Conn, error) {
						t.Fatal("configured egress leaked onto host route")
						return nil, nil
					},
					broker: func(port int) (proxy.EgressDialer, error) {
						if port != cfg.Port {
							t.Fatalf("broker port = %d", port)
						}
						return dotsTestDial(func(_ context.Context, network, target string) (net.Conn, error) {
							calls++
							if network != "tcp" || target != address {
								t.Fatalf("unexpected upstream %s %s", network, target)
							}
							var conn net.Conn
							conn, peer = net.Pipe()
							return conn, nil
						}), nil
					},
				}
				conn, err := n.DialContext(context.Background(), "tcp", address)
				if err != nil || calls != 1 {
					t.Fatalf("broker calls=%d error=%v", calls, err)
				}
				conn.Close()
				peer.Close()
			}
		})
	}
}

func TestDotsNetworkRetainsLocalAndUnrelatedRoutes(t *testing.T) {
	for _, address := range []string{"127.0.0.1:8000", "[::1]:8000", "unrelated.example:443", "chatgpt.com.evil.test:443"} {
		t.Run(address, func(t *testing.T) {
			called := false
			n := dotsNetwork{
				load: func() (*proxy.Config, error) {
					t.Fatal("local or unrelated route depends on CQ configuration")
					return nil, nil
				},
				direct: func(context.Context, string, string) (net.Conn, error) {
					called = true
					return nil, net.ErrClosed
				},
			}
			_, err := n.DialContext(context.Background(), "tcp", address)
			if !called || !errors.Is(err, net.ErrClosed) {
				t.Fatalf("existing route not used: %v", err)
			}
		})
	}
}

func TestDotsNetworkBlocksFailuresWithoutDirectFallback(t *testing.T) {
	for _, failure := range []string{"config", "missing", "nil_config", "descriptor", "nil_broker", "tunnel"} {
		t.Run(failure, func(t *testing.T) {
			secret := errors.New("private credential must not appear")
			n := dotsNetwork{
				load: func() (*proxy.Config, error) {
					if failure == "missing" {
						return nil, &os.PathError{Op: "open", Path: "/private/proxy.json", Err: os.ErrNotExist}
					}
					if failure == "config" {
						return nil, secret
					}
					if failure == "nil_config" {
						return nil, nil
					}
					return &proxy.Config{Port: 19280, CodexWireGuardProfile: "/private/us.conf"}, nil
				},
				broker: func(int) (proxy.EgressDialer, error) {
					if failure == "descriptor" {
						return nil, secret
					}
					if failure == "nil_broker" {
						return nil, nil
					}
					return dotsTestDial(func(context.Context, string, string) (net.Conn, error) { return nil, secret }), nil
				},
				direct: func(context.Context, string, string) (net.Conn, error) {
					t.Fatal("failed tunnel leaked onto host route")
					return nil, nil
				},
			}
			_, err := n.DialContext(context.Background(), "tcp", "chatgpt.com:443")
			if err == nil || strings.Contains(err.Error(), "private") {
				t.Fatalf("unsafe failure: %v", err)
			}
		})
	}
}

func TestDotsNetworkAllowsExplicitDirectOrDefaultConfiguration(t *testing.T) {
	for _, mode := range []string{"default", "direct"} {
		t.Run(mode, func(t *testing.T) {
			n := dotsNetwork{
				load: func() (*proxy.Config, error) {
					cfg := &proxy.Config{}
					if mode == "direct" {
						cfg.CodexEgressProxy = "direct"
					}
					return cfg, nil
				},
				broker: func(int) (proxy.EgressDialer, error) { t.Fatal("unexpected broker"); return nil, nil },
				direct: func(context.Context, string, string) (net.Conn, error) { return nil, net.ErrClosed },
			}
			_, err := n.DialContext(context.Background(), "tcp", "chatgpt.com:443")
			if !errors.Is(err, net.ErrClosed) {
				t.Fatalf("existing direct route changed: %v", err)
			}
		})
	}
}

func TestDotsNetworkReloadsDescriptorAndHonoursCancellation(t *testing.T) {
	opens := 0
	n := dotsNetwork{
		load: func() (*proxy.Config, error) {
			return &proxy.Config{Port: 19280, CodexWireGuardProfile: "/private/us.conf"}, nil
		},
		broker: func(int) (proxy.EgressDialer, error) {
			opens++
			return dotsTestDial(func(context.Context, string, string) (net.Conn, error) { return nil, net.ErrClosed }), nil
		},
		direct: func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("unexpected fallback")
			return nil, nil
		},
	}
	for i := 0; i < 2; i++ {
		_, _ = n.DialContext(context.Background(), "tcp", "chatgpt.com:443")
	}
	if opens != 2 {
		t.Fatalf("stale descriptor reused: opens=%d", opens)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := n.DialContext(ctx, "tcp", "chatgpt.com:443")
	if !errors.Is(err, context.Canceled) || opens != 2 {
		t.Fatalf("cancelled connection touched broker: opens=%d err=%v", opens, err)
	}
}

func TestDotsNetworkBlocksDeletedPolicyAfterTunnelWasConfigured(t *testing.T) {
	loads, opens := 0, 0
	n := dotsNetwork{
		load: func() (*proxy.Config, error) {
			loads++
			if loads > 1 {
				return nil, os.ErrNotExist
			}
			return &proxy.Config{Port: 19280, CodexWireGuardProfile: "/private/us.conf"}, nil
		},
		broker: func(int) (proxy.EgressDialer, error) {
			opens++
			return dotsTestDial(func(context.Context, string, string) (net.Conn, error) {
				conn, peer := net.Pipe()
				peer.Close()
				return conn, nil
			}), nil
		},
		direct: func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("deleted policy leaked traffic")
			return nil, nil
		},
	}
	conn, err := n.DialContext(context.Background(), "tcp", "chatgpt.com:443")
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if conn, err = n.DialContext(context.Background(), "tcp", "chatgpt.com:443"); err == nil || conn != nil || opens != 1 {
		t.Fatalf("deleted policy accepted: opens=%d error=%v", opens, err)
	}
}
