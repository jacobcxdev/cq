package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"sync"

	"github.com/jacobcxdev/cq/internal/nativeusage"
)

const nativeUsageHelloLimit = 64 << 10

var errNativeUsageHelloDone = errors.New("native usage client hello inspected")

// routeNativeUsage selects only the two usage services from a bounded TLS hello.
// The original connection stays available for every other protocol or hostname.
func routeNativeUsage(ctx context.Context, original net.Conn, config *tls.Config, serve func(context.Context, net.Conn, string, *tls.Config)) net.Conn {
	client, downstream := net.Pipe()
	life, cancel := context.WithCancel(ctx)
	result := &nativeUsageRouteConn{Conn: client, original: original, cancel: cancel}
	go func() {
		defer func() { _ = recover() }()
		defer cancel()
		defer downstream.Close()
		defer original.Close()
		stop := context.AfterFunc(life, func() { _ = downstream.Close(); _ = original.Close() })
		defer stop()

		hellos := make(chan nativeUsageHello, 1)
		origins := make(chan nativeUsageFirstRead, 1)
		var readers sync.WaitGroup
		readers.Add(2)
		defer func() { _ = downstream.Close(); _ = original.Close(); readers.Wait() }()
		go func() {
			defer readers.Done()
			defer close(hellos)
			defer func() {
				if recover() != nil {
					cancel()
				}
			}()
			hellos <- inspectNativeUsageHello(life, downstream)
		}()
		go func() {
			defer readers.Done()
			defer close(origins)
			defer func() {
				if recover() != nil {
					cancel()
				}
			}()
			data := make([]byte, 32<<10)
			n, err := original.Read(data)
			origins <- nativeUsageFirstRead{data[:n], err}
		}()

		var hello *nativeUsageHello
		var first *nativeUsageFirstRead
		select {
		case <-life.Done():
			return
		case value := <-origins:
			first = &value
		case value := <-hellos:
			hello = &value
			if nativeusage.IsHost(value.host) && serve != nil {
				// A server-first protocol must retain its original connection.
				select {
				case value := <-origins:
					first = &value
				default:
				}
				if first == nil || len(first.data) == 0 {
					_ = original.Close()
					serve(life, &nativeUsageReplayConn{Conn: downstream, reader: io.MultiReader(bytes.NewReader(value.prefix), downstream)}, net.JoinHostPort(value.host, "443"), config)
					return
				}
			}
		}
		forwardNativeUsageOriginal(downstream, original, hello, first, hellos, origins)
	}()
	return result
}

type nativeUsageRouteConn struct {
	net.Conn
	original net.Conn
	cancel   context.CancelFunc
}

func (c *nativeUsageRouteConn) Close() error {
	c.cancel()
	_ = c.original.Close()
	return c.Conn.Close()
}

type nativeUsageFirstRead struct {
	data []byte
	err  error
}

type nativeUsageHello struct {
	host    string
	prefix  []byte
	readErr error
}

func inspectNativeUsageHello(ctx context.Context, conn net.Conn) nativeUsageHello {
	probe := &nativeUsageHelloConn{Conn: conn}
	var host string
	server := tls.Server(probe, &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		host = hello.ServerName
		return nil, errNativeUsageHelloDone
	}})
	if err := server.HandshakeContext(ctx); !errors.Is(err, errNativeUsageHelloDone) {
		host = ""
	}
	return nativeUsageHello{host, probe.prefix.Bytes(), probe.readErr}
}

// The probe consumes only a bounded hello. Its synthetic replies never reach
// the client, and every consumed byte is replayed into the selected connection.
type nativeUsageHelloConn struct {
	net.Conn
	prefix  bytes.Buffer
	readErr error
}

func (c *nativeUsageHelloConn) Read(p []byte) (int, error) {
	remaining := nativeUsageHelloLimit - c.prefix.Len()
	if remaining == 0 {
		return 0, errNativeUsageHelloDone
	}
	if c.prefix.Len() == 0 {
		p = p[:min(len(p), 1)]
	} else {
		p = p[:min(len(p), remaining)]
	}
	n, err := c.Conn.Read(p)
	_, _ = c.prefix.Write(p[:n])
	c.readErr = err
	if c.prefix.Len() == 1 && c.prefix.Bytes()[0] != 22 {
		return n, errNativeUsageHelloDone
	}
	return n, err
}

func (c *nativeUsageHelloConn) Write(p []byte) (int, error) { return len(p), nil }
func (c *nativeUsageHelloConn) Close() error                { return nil }

type nativeUsageReplayConn struct {
	net.Conn
	reader io.Reader
}

func (c *nativeUsageReplayConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func forwardNativeUsageOriginal(client, original net.Conn, hello *nativeUsageHello, first *nativeUsageFirstRead, hellos <-chan nativeUsageHello, origins <-chan nativeUsageFirstRead) {
	closeBoth := func() { _ = client.Close(); _ = original.Close() }
	var sender sync.WaitGroup
	sender.Add(1)
	go func() {
		defer sender.Done()
		defer closeBoth()
		defer func() { _ = recover() }()
		if hello == nil {
			value := <-hellos
			hello = &value
		}
		if len(hello.prefix) > 0 {
			if _, err := original.Write(hello.prefix); err != nil {
				return
			}
		}
		if hello.readErr != nil {
			return
		}
		_, _ = io.Copy(original, client)
	}()
	func() {
		defer closeBoth()
		defer func() { _ = recover() }()
		if first == nil {
			value := <-origins
			first = &value
		}
		if len(first.data) > 0 {
			if _, err := client.Write(first.data); err != nil {
				return
			}
		}
		if first.err != nil {
			return
		}
		_, _ = io.Copy(client, original)
	}()
	sender.Wait()
}
