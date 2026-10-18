package dots

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProbeRemoteControlOpaqueUpgradeBidirectional(t *testing.T) {
	for _, status := range []int{101, 426} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			p, _, _ := probeFixtureForTest(t)
			observed := make(chan string, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.RequestURI() != "/backend-api/wham/remote/control/server" || r.Header.Get("Authorization") != "Bearer native-token" || r.Header.Get("ChatGPT-Account-Id") != "system" {
					t.Error("native upgrade identity changed")
				}
				connection, buffer, err := w.(http.Hijacker).Hijack()
				if err != nil {
					return
				}
				defer connection.Close()
				if status != 101 {
					_, _ = fmt.Fprintf(buffer, "HTTP/1.1 426 Upgrade Required\r\nContent-Length: 0\r\n\r\n")
					_ = buffer.Flush()
					return
				}
				// First opaque bytes share the same packet as the handshake.
				_, _ = buffer.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\nserver-first")
				_ = buffer.Flush()
				body := make([]byte, len("masked-ping-close"))
				_, err = io.ReadFull(buffer, body)
				if err == nil {
					observed <- string(body)
					_, _ = buffer.WriteString("server-last")
					_ = buffer.Flush()
				}
			}))
			defer upstream.Close()
			p.options.Origin = upstream.URL
			proxy := httptest.NewServer(p)
			defer proxy.Close()
			connection, err := net.Dial("tcp", strings.TrimPrefix(proxy.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
			_, _ = io.WriteString(connection, "GET /backend-api/wham/remote/control/server HTTP/1.1\r\nHost: chatgpt.com\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nAuthorization: Bearer native-token\r\nChatGPT-Account-Id: system\r\n\r\n")
			reader := bufio.NewReader(connection)
			response, err := http.ReadResponse(reader, nil)
			if err != nil || response.StatusCode != status {
				t.Fatalf("handshake %v %v", response, err)
			}
			if status != 101 {
				return
			}
			first := make([]byte, len("server-first"))
			if _, err := io.ReadFull(reader, first); err != nil || string(first) != "server-first" {
				t.Fatal("buffered server frame lost")
			}
			_, _ = io.WriteString(connection, "masked-ping-close")
			last := make([]byte, len("server-last"))
			if _, err := io.ReadFull(reader, last); err != nil || string(last) != "server-last" {
				t.Fatal("downstream frame lost")
			}
			if got := <-observed; got != "masked-ping-close" {
				t.Fatal("native frame altered")
			}
		})
	}
}

func TestProbeRemoteControlExactEndpointAndBody(t *testing.T) {
	p, _, _ := probeFixtureForTest(t)
	calls := 0
	p.options.HTTP = probeDoer(func(*http.Request) (*http.Response, error) { calls++; return probeSuccess("{}"), nil })
	for _, path := range []string{"/backend-api/wham/remote/control/server?unexpected=1", "/backend-api/wham/remote/control/server/"} {
		r := probeRequest("GET", path, "")
		r.Header.Set("Upgrade", "websocket")
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	if calls != 2 {
		t.Fatal("nonexact endpoint upgraded")
	}
	r := probeRequest("GET", "/backend-api/wham/remote/control/server", "body")
	r.Header.Set("Upgrade", "websocket")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != 400 || calls != 2 {
		t.Fatal("upgrade body accepted")
	}
}

func TestProbeEventStreamFlushesAndCancels(t *testing.T) {
	p, _, _ := probeFixtureForTest(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	p.options.Origin = upstream.URL
	p.options.HTTP = probeHTTPClient()
	server := httptest.NewServer(p)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/backend-api/tbo/profile-owner/activity/stream", nil)
	r.Header = probeRequest("GET", "/", "").Header
	response, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	line, err := bufio.NewReader(response.Body).ReadString('\n')
	if err != nil || line != "data: first\n" {
		t.Fatalf("stream not flushed: %q %v", line, err)
	}
	cancel()
}

func TestProbeDefaultTransportHasNoStreamingTimeoutOrWriteRetry(t *testing.T) {
	client := probeHTTPClient()
	transport := client.Transport.(*http.Transport)
	if client.Timeout != 0 || !transport.DisableKeepAlives || transport.Proxy != nil || transport.ResponseHeaderTimeout == 0 || transport.TLSHandshakeTimeout == 0 {
		t.Fatal("unsafe streaming/retry transport")
	}
	if transport.TLSClientConfig != nil && transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("public TLS verification disabled")
	}
}
