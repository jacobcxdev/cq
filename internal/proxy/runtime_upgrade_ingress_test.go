package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestRuntimeUpgradeWaitsForSSETerminal(t *testing.T) {
	gate := NewRuntimeUpgradeAdmission()
	started := make(chan struct{})
	terminal := make(chan struct{})
	server := httptest.NewServer(RuntimeUpgradeHTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: started\n\n"))
		w.(http.Flusher).Flush()
		close(started)
		<-terminal
		w.Write([]byte("data: completed\n\n"))
	}), gate))
	defer server.Close()
	response, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	<-started
	if err := gate.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := gate.AwaitQuiescence(ctx); err == nil {
		t.Fatal("quiescence acknowledged before SSE terminal")
	}
	close(terminal)
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	if err := gate.AwaitQuiescence(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestRuntimeUpgradeBoundaryAdmissionRace(t *testing.T) {
	gate := NewRuntimeUpgradeAdmission()
	var wait sync.WaitGroup
	start := make(chan struct{})
	for range 100 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			if release, err := gate.BeginTurn(); err == nil {
				defer release()
			}
		}()
	}
	close(start)
	if err := gate.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	wait.Wait()
	for range 100 {
		if release, err := gate.BeginTurn(); err == nil {
			release()
			t.Fatal("turn admitted after pause acknowledgement")
		}
	}
	if err := gate.AwaitQuiescence(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestRuntimeUpgradeDeadlineRestoresAdmissions(t *testing.T) {
	gate := NewRuntimeUpgradeAdmission()
	release, err := gate.BeginRequest()
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := gate.AwaitQuiescence(ctx); err == nil {
		t.Fatal("active request ignored")
	}
	gate.Resume()
	release()
	fresh, err := gate.BeginRequest()
	if err != nil {
		t.Fatal("deferred upgrade blocked fresh request")
	}
	fresh()
}
func TestRuntimeUpgradeListenerPauseRetainsBacklog(t *testing.T) {
	tcp, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	listener := NewRuntimeUpgradeListener(tcp)
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() { conn, _ := listener.Accept(); accepted <- conn }()
	if err := listener.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal("paused listener refused connection")
	}
	defer client.Close()
	select {
	case conn := <-accepted:
		if conn != nil {
			conn.Close()
		}
		t.Fatal("old server accepted after pause")
	case <-time.After(20 * time.Millisecond):
	}
	listener.Resume()
	select {
	case conn := <-accepted:
		if conn == nil {
			t.Fatal("missing connection")
		}
		conn.Close()
	case <-time.After(time.Second):
		t.Fatal("resume did not drain retained backlog")
	}
}

func TestRuntimeUpgradeTracksAcceptedSlowHeaders(t *testing.T) {
	tcp, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	listener := NewRuntimeUpgradeListener(tcp)
	connections := NewRuntimeUpgradeConnections()
	listener.SetConnectionTracker(connections)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }), ConnState: connections.ConnState, ReadHeaderTimeout: time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	defer func() { server.Close(); <-done }()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.Write([]byte("GET / HTTP/1.1\r\nHost:"))
	deadline := time.Now().Add(time.Second)
	for connections.Admission.quiescent() {
		if time.Now().After(deadline) {
			t.Fatal("accepted slow headers not counted")
		}
		time.Sleep(time.Millisecond)
	}
	if err := listener.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := connections.Admission.AwaitQuiescence(short); err == nil {
		t.Fatal("slow headers disappeared during pause")
	}
	client.Write([]byte(" localhost\r\nConnection: close\r\n\r\n"))
	if _, err := io.ReadAll(client); err != nil {
		t.Fatal(err)
	}
	wait, cancelWait := context.WithTimeout(context.Background(), time.Second)
	defer cancelWait()
	if err := connections.Admission.AwaitQuiescence(wait); err != nil {
		t.Fatal(err)
	}
	listener.Resume()
}

func TestRuntimeUpgradePauseDrainsAcceptedBoundaryBeforeHandoff(t *testing.T) {
	tcp, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	listener := NewRuntimeUpgradeListener(tcp)
	connections := NewRuntimeUpgradeConnections()
	listener.SetConnectionTracker(connections)
	accepted := make(chan struct{})
	releaseAccept := make(chan struct{})
	var once sync.Once
	listener.acceptTCP = func() (net.Conn, error) {
		conn, err := tcp.Accept()
		if conn != nil {
			once.Do(func() { close(accepted); <-releaseAccept })
		}
		return conn, err
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "previous") }), ConnState: connections.ConnState}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	defer func() { listener.Resume(); server.Close(); <-done }()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := client.Write([]byte("GET / HTTP/1.1\r\nHost:")); err != nil {
		t.Fatal(err)
	}
	<-accepted
	paused := make(chan error, 1)
	go func() { paused <- listener.Pause(context.Background()) }()
	deadline := time.Now().Add(time.Second)
	for {
		listener.mu.Lock()
		isPaused := listener.paused
		listener.mu.Unlock()
		if isPaused {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pause did not begin")
		}
		time.Sleep(time.Millisecond)
	}
	close(releaseAccept)
	if err := <-paused; err != nil {
		t.Fatal(err)
	}
	if connections.Admission.quiescent() {
		t.Fatal("pause acknowledged an untracked kernel-accepted connection")
	}
	if _, err := client.Write([]byte(" localhost\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(body) != "previous" {
		t.Fatalf("accepted turn lost before handoff: %q %v", body, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := connections.Admission.AwaitQuiescence(ctx); err != nil {
		t.Fatal(err)
	}
	file, err := listener.File()
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	successor, err := net.FileListener(file)
	if err != nil {
		t.Fatal(err)
	}
	defer successor.Close()
	listener.Close()
	nextServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "successor") })}
	nextDone := make(chan error, 1)
	go func() { nextDone <- nextServer.Serve(successor) }()
	defer func() { nextServer.Close(); <-nextDone }()
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	next, err := (&http.Client{Transport: transport, Timeout: time.Second}).Get("http://" + successor.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	nextBody, err := io.ReadAll(next.Body)
	next.Body.Close()
	if err != nil || string(nextBody) != "successor" {
		t.Fatalf("retained handoff failed: %q %v", nextBody, err)
	}
}
