package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacobcxdev/cq/internal/httputil"
)

// The fixture accepts only a remote DNS CONNECT request. It never resolves the
// synthetic target hostname and instead forwards to the isolated local origin.
func socksFixture(t *testing.T, origin string, reject, block bool) (string, <-chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	names := make(chan string, 16)
	var connections sync.WaitGroup
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop); listener.Close(); connections.Wait() })
	connections.Add(1)
	go func() {
		defer connections.Done()
		defer func() { _ = recover() }()
		for {
			c, e := listener.Accept()
			if e != nil {
				return
			}
			connections.Add(1)
			go func() {
				defer connections.Done()
				defer c.Close()
				defer func() { _ = recover() }()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				header := make([]byte, 2)
				if _, e := io.ReadFull(c, header); e != nil {
					return
				}
				methods := make([]byte, int(header[1]))
				if _, e := io.ReadFull(c, methods); e != nil {
					return
				}
				if block {
					<-stop
					return
				}
				if reject {
					_, _ = c.Write([]byte{5, 255})
					return
				}
				if _, e := c.Write([]byte{5, 0}); e != nil {
					return
				}
				request := make([]byte, 5)
				if _, e := io.ReadFull(c, request); e != nil || request[0] != 5 || request[1] != 1 || request[3] != 3 {
					return
				}
				name := make([]byte, int(request[4]))
				if _, e := io.ReadFull(c, name); e != nil {
					return
				}
				port := make([]byte, 2)
				if _, e := io.ReadFull(c, port); e != nil {
					return
				}
				_ = binary.BigEndian.Uint16(port)
				names <- string(name)
				upstream, e := net.DialTimeout("tcp", origin, time.Second)
				if e != nil {
					return
				}
				defer upstream.Close()
				if _, e := c.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}); e != nil {
					return
				}
				done := make(chan struct{})
				go func() {
					defer close(done)
					defer func() { _ = recover() }()
					_, _ = io.Copy(upstream, c)
					_ = upstream.Close()
				}()
				_, _ = io.Copy(c, upstream)
				_ = c.Close()
				<-done
			}()
		}
	}()
	return listener.Addr().String(), names
}

func TestSOCKSEgressRemoteDNSHTTPStreamingAndWebSocket(t *testing.T) {
	var calls atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer synthetic-selected" {
			t.Error("selected authorization changed")
		}
		if r.URL.Path == "/ws" {
			c, e := (&websocket.Upgrader{}).Upgrade(w, r, nil)
			if e != nil {
				return
			}
			defer c.Close()
			_ = c.WriteMessage(websocket.TextMessage, []byte("synthetic"))
			return
		}
		if r.URL.Path == "/sse" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: synthetic\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, "synthetic")
	}))
	defer origin.Close()
	originURL, _ := url.Parse(origin.URL)
	address, names := socksFixture(t, originURL.Host, false, false)
	for _, scheme := range []string{"socks5", "socks5h"} {
		t.Run(scheme, func(t *testing.T) {
			u, _ := url.Parse(scheme + "://" + address)
			egress, err := newSOCKSEgress(u)
			if err != nil {
				t.Fatal(err)
			}
			transport := egress.HTTP.(*http.Transport)
			defer transport.CloseIdleConnections()
			transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // Local synthetic TLS only.
			egress.WebSocket.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
			client := &http.Client{Transport: egress.HTTP, Timeout: 3 * time.Second}
			for _, path := range []string{"/http", "/sse"} {
				ctx, cancel := context.WithCancel(context.Background())
				request, _ := http.NewRequestWithContext(ctx, "GET", "https://only-via-socks.invalid:"+originURL.Port()+path, nil)
				request.Header.Set("Authorization", "Bearer synthetic-selected")
				response, e := client.Do(request)
				if e != nil {
					cancel()
					t.Fatal(e)
				}
				if path == "/sse" {
					line, e := bufio.NewReader(response.Body).ReadString('\n')
					if e != nil || line != "data: synthetic\n" {
						t.Error("SSE did not stream")
					}
				} else {
					body, e := httputil.ReadBody(response.Body)
					if e != nil || string(body) != "synthetic" {
						t.Error("HTTP payload changed")
					}
				}
				cancel()
				response.Body.Close()
			}
			c, response, e := egress.WebSocket.Dial("wss://only-via-socks.invalid:"+originURL.Port()+"/ws", http.Header{"Authorization": {"Bearer synthetic-selected"}})
			if response != nil && response.Body != nil {
				defer response.Body.Close()
			}
			if e != nil {
				t.Fatal(e)
			}
			defer c.Close()
			_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
			_, body, e := c.ReadMessage()
			if e != nil || string(body) != "synthetic" {
				t.Error("WebSocket payload changed")
			}
		})
	}
	if calls.Load() != 6 {
		t.Fatal("unexpected origin request count")
	}
	count := len(names)
	if count < 4 {
		t.Fatal("missing remote DNS requests")
	}
	for i := 0; i < count; i++ {
		if <-names != "only-via-socks.invalid" {
			t.Fatal("SOCKS target was locally resolved")
		}
	}
}

func TestSOCKSEgressHandshakeFailureAndCancellation(t *testing.T) {
	var calls atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer origin.Close()
	for _, block := range []bool{false, true} {
		t.Run(map[bool]string{false: "reject", true: "cancel"}[block], func(t *testing.T) {
			address, _ := socksFixture(t, "", !block, block)
			u, _ := url.Parse("socks5://" + address)
			egress, e := newSOCKSEgress(u)
			if e != nil {
				t.Fatal(e)
			}
			defer egress.HTTP.(*http.Transport).CloseIdleConnections()
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			request, _ := http.NewRequestWithContext(ctx, "GET", origin.URL, nil)
			start := time.Now()
			response, e := (&http.Client{Transport: egress.HTTP}).Do(request)
			if response != nil {
				response.Body.Close()
			}
			if e == nil {
				t.Fatal("failed SOCKS handshake accepted")
			}
			if time.Since(start) > time.Second {
				t.Fatal("blocked handshake ignored cancellation")
			}
			wsCtx, wsCancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer wsCancel()
			c, response, e := egress.WebSocket.DialContext(wsCtx, "ws"+strings.TrimPrefix(origin.URL, "http"), nil)
			if c != nil {
				c.Close()
			}
			if response != nil && response.Body != nil {
				response.Body.Close()
			}
			if e == nil {
				t.Fatal("failed SOCKS WebSocket handshake accepted")
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("SOCKS failure fell back to direct origin")
	}
}

func TestSOCKSEgressRejectsInvalidConstructorInput(t *testing.T) {
	for _, u := range []*url.URL{nil, {Scheme: "http", Host: "localhost:1080"}, {Scheme: "socks5"}, {Scheme: "socks5", Host: "localhost:1080", User: url.UserPassword("synthetic-user", "synthetic-secret")}} {
		egress, err := newSOCKSEgress(u)
		if egress != nil || err == nil {
			t.Fatal("invalid SOCKS constructor input accepted")
		}
		if strings.Contains(err.Error(), "synthetic-") {
			t.Fatal("SOCKS constructor error exposed credentials")
		}
	}
}
