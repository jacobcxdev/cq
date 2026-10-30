package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

// ProxyFromEnvironment caches its first environment snapshot, so each case
// exercises the manager in a fresh process with only synthetic proxy settings.
func TestEgressManagerAmbientProxyScheme(t *testing.T) {
	const marker = "CQ_TEST_AMBIENT_PROXY_SCHEME"
	const banner = "synthetic ambient CONNECT established"
	if scheme := os.Getenv(marker); scheme != "" {
		// An incorrect direct route must fail without consulting real origin DNS.
		net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("origin DNS forbidden in synthetic proxy test")
		}}
		port := "443"
		if scheme == "http" {
			port = "80"
		}
		manager, err := NewCodexEgressManager(&Config{CodexUpstream: scheme + "://origin.invalid"}, fsutil.NewMemFS())
		if err != nil {
			t.Fatal(err)
		}
		defer manager.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		conn, err := manager.DialContext(ctx, "tcp", "origin.invalid:"+port)
		if err != nil {
			t.Fatalf("ambient %s proxy dial: %v", scheme, err)
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		got := make([]byte, len(banner))
		if _, err := io.ReadFull(conn, got); err != nil {
			t.Fatal(err)
		}
		if string(got) != banner {
			t.Fatalf("CONNECT banner = %q", got)
		}
		return
	}
	for _, scheme := range []string{"http", "https"} {
		t.Run(scheme, func(t *testing.T) {
			port := "443"
			if scheme == "http" {
				port = "80"
			}
			var connections atomic.Int32
			var handlers sync.WaitGroup
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handlers.Add(1)
				defer handlers.Done()
				defer func() {
					if p := recover(); p != nil {
						t.Errorf("synthetic CONNECT handler panic: %v", p)
					}
				}()
				if r.Method != http.MethodConnect || r.Host != "origin.invalid:"+port {
					http.Error(w, "unexpected CONNECT target", http.StatusBadRequest)
					return
				}
				conn, buffered, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Errorf("hijack: %v", err)
					return
				}
				defer conn.Close()
				_ = conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
				connections.Add(1)
				if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n" + banner); err != nil {
					t.Errorf("CONNECT response: %v", err)
					return
				}
				if err := buffered.Flush(); err != nil {
					t.Errorf("CONNECT flush: %v", err)
				}
			}))
			defer func() { proxy.Close(); handlers.Wait() }()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestEgressManagerAmbientProxyScheme$", "-test.count=1")
			// Do not inherit credentials, profiles, ambient proxy settings, or CGI state.
			command.Env = []string{marker + "=" + scheme, "HTTP_PROXY=", "http_proxy=", "HTTPS_PROXY=", "https_proxy=", "ALL_PROXY=", "all_proxy=", "NO_PROXY=", "no_proxy="}
			variable := "HTTPS_PROXY"
			if scheme == "http" {
				variable = "HTTP_PROXY"
			}
			for i, entry := range command.Env {
				if strings.HasPrefix(entry, variable+"=") {
					command.Env[i] = variable + "=" + proxy.URL
				}
			}
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("ambient %s subprocess: %v\n%s", scheme, err, output)
			}
			if connections.Load() != 1 {
				t.Fatalf("CONNECT requests = %d", connections.Load())
			}
		})
	}
}
