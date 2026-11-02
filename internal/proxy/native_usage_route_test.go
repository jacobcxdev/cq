package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/nativeusage"
)

func nativeUsageRouteConfigs(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "usage")
	fs := fsutil.OSFileSystem{}
	info, err := nativeusage.Prepare(root, fs)
	if err != nil {
		t.Fatal(err)
	}
	if err := nativeusage.SetEnabled(root, fs, true); err != nil {
		t.Fatal(err)
	}
	server, err := nativeusage.LoadTLS(root, fs)
	if err != nil {
		t.Fatal(err)
	}
	pem, err := os.ReadFile(info.CertificatePath)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		t.Fatal("could not load route test certificate")
	}
	return server, &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
}

func TestNativeUsageRouteSelectsTLSService(t *testing.T) {
	serverTLS, clientTLS := nativeUsageRouteConfigs(t)
	for _, host := range []string{"chatgpt.com", "ws.chatgpt.com"} {
		for _, fragment := range []bool{false, true} {
			t.Run(host+map[bool]string{false: "/whole", true: "/fragmented"}[fragment], func(t *testing.T) {
				original, origin := net.Pipe()
				defer origin.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				selected := make(chan string, 1)
				front := routeNativeUsage(ctx, original, serverTLS, func(ctx context.Context, conn net.Conn, target string, config *tls.Config) {
					selected <- target
					peer := tls.Server(conn, config)
					defer peer.Close()
					if peer.HandshakeContext(ctx) != nil {
						return
					}
					var input [4]byte
					if _, err := io.ReadFull(peer, input[:]); err == nil {
						_, _ = peer.Write(append([]byte("adapted:"), input[:]...))
					}
				})
				defer front.Close()
				if fragment {
					front = &nativeUsageFragmentConn{Conn: front}
				}
				config := clientTLS.Clone()
				config.ServerName = host
				client := tls.Client(front, config)
				if err := client.HandshakeContext(ctx); err != nil {
					t.Fatal(err)
				}
				if _, err := client.Write([]byte("ping")); err != nil {
					t.Fatal(err)
				}
				output := make([]byte, len("adapted:ping"))
				if _, err := io.ReadFull(client, output); err != nil {
					t.Fatal(err)
				}
				if string(output) != "adapted:ping" {
					t.Fatalf("response %q", output)
				}
				if got := <-selected; got != net.JoinHostPort(host, "443") {
					t.Fatalf("selected %q", got)
				}
				_ = origin.SetReadDeadline(time.Now().Add(time.Second))
				if _, err := origin.Read(make([]byte, 1)); err != io.EOF {
					t.Fatalf("unused origin read: %v", err)
				}
			})
		}
	}
}

type nativeUsageFragmentConn struct{ net.Conn }

func (c *nativeUsageFragmentConn) Write(p []byte) (int, error) {
	var total int
	for len(p) > 0 {
		n, err := c.Conn.Write(p[:min(len(p), 7)])
		total += n
		p = p[n:]
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func nativeUsageRouteHello(t *testing.T, host string) []byte {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = recover() }()
		_ = tls.Client(client, &tls.Config{ServerName: host}).Handshake()
	}()
	_ = server.SetReadDeadline(time.Now().Add(time.Second))
	var header [5]byte
	if _, err := io.ReadFull(server, header[:]); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, int(binary.BigEndian.Uint16(header[3:])))
	if _, err := io.ReadFull(server, data); err != nil {
		t.Fatal(err)
	}
	_ = server.Close()
	_ = client.Close()
	<-done
	return append(header[:], data...)
}

func TestNativeUsageRoutePreservesUnselectedBytes(t *testing.T) {
	large := []byte{1, 0, 255, 252}
	large = append(large, make([]byte, 65532)...)
	var oversized bytes.Buffer
	for len(large) > 0 {
		n := min(len(large), 16<<10)
		_, _ = oversized.Write([]byte{22, 3, 3, byte(n >> 8), byte(n)})
		_, _ = oversized.Write(large[:n])
		large = large[n:]
	}
	for _, test := range []struct {
		name  string
		input []byte
	}{
		{"other SNI", nativeUsageRouteHello(t, "other.example.invalid")},
		{"no SNI", nativeUsageRouteHello(t, "203.0.113.7")},
		{"plain", []byte("plaintext request with private detail")},
		{"malformed", []byte{22, 3, 3, 0, 4, 2, 0, 0, 0}},
		{"oversized", oversized.Bytes()},
	} {
		t.Run(test.name, func(t *testing.T) {
			original, origin := net.Pipe()
			defer origin.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var adapted atomic.Bool
			front := routeNativeUsage(ctx, original, nil, func(context.Context, net.Conn, string, *tls.Config) { adapted.Store(true) })
			defer front.Close()
			_ = front.SetDeadline(time.Now().Add(2 * time.Second))
			received := make(chan []byte, 1)
			go func() {
				defer func() { _ = recover() }()
				input := make([]byte, len(test.input))
				_, err := io.ReadFull(origin, input)
				if err == nil {
					_, _ = origin.Write([]byte("opaque"))
				}
				received <- input
			}()
			if _, err := front.Write(test.input); err != nil {
				t.Fatal(err)
			}
			output := make([]byte, len("opaque"))
			if _, err := io.ReadFull(front, output); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(<-received, test.input) || string(output) != "opaque" {
				t.Fatal("opaque bytes changed")
			}
			if adapted.Load() {
				t.Fatal("unselected connection adapted")
			}
		})
	}
}

func TestNativeUsageRoutePreservesServerFirst(t *testing.T) {
	original, origin := net.Pipe()
	defer origin.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var adapted atomic.Bool
	front := routeNativeUsage(ctx, original, nil, func(context.Context, net.Conn, string, *tls.Config) { adapted.Store(true) })
	defer front.Close()
	_ = front.SetDeadline(time.Now().Add(2 * time.Second))
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = recover() }()
		if _, err := origin.Write([]byte("banner")); err != nil {
			return
		}
		var reply [1]byte
		if _, err := io.ReadFull(origin, reply[:]); err == nil {
			_, _ = origin.Write(reply[:])
		}
	}()
	banner := make([]byte, 6)
	if _, err := io.ReadFull(front, banner); err != nil {
		t.Fatal(err)
	}
	if string(banner) != "banner" {
		t.Fatalf("banner %q", banner)
	}
	if _, err := front.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	var reply [1]byte
	if _, err := io.ReadFull(front, reply[:]); err != nil || reply[0] != 'x' {
		t.Fatalf("reply %q: %v", reply, err)
	}
	<-done
	if adapted.Load() {
		t.Fatal("server-first connection adapted")
	}
}

func TestNativeUsageRouteCancellationAndDeadline(t *testing.T) {
	for _, closeClient := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "close"}[closeClient], func(t *testing.T) {
			original, origin := net.Pipe()
			defer origin.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			front := routeNativeUsage(ctx, original, nil, nil)
			defer front.Close()
			_ = front.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
			_, err := front.Read(make([]byte, 1))
			if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
				t.Fatalf("deadline error %v", err)
			}
			if closeClient {
				_ = front.Close()
			} else {
				cancel()
			}
			_ = origin.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := origin.Read(make([]byte, 1)); err != io.EOF {
				t.Fatalf("origin remained open: %v", err)
			}
		})
	}
}
