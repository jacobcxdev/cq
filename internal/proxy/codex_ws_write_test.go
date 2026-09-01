package proxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWriteCodexWSMessageCancellationUnblocksGorillaWriter(t *testing.T) {
	serverConnection := make(chan *websocket.Conn, 1)
	releaseServer := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(writer, request, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		serverConnection <- connection
		<-releaseServer
		_ = connection.Close()
	}))
	t.Cleanup(func() {
		close(releaseServer)
		server.Close()
	})

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	peer := <-serverConnection
	if tcpConnection, ok := client.NetConn().(*net.TCPConn); ok {
		if err := tcpConnection.SetWriteBuffer(1024); err != nil {
			t.Fatal(err)
		}
	}

	connection := &codexWSSignallingWriteConn{Conn: client, started: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- writeCodexWSMessage(ctx, connection, websocket.BinaryMessage, make([]byte, 16<<20))
	}()
	select {
	case <-connection.started:
	case <-time.After(time.Second):
		t.Fatal("WebSocket write did not start")
	}
	select {
	case err := <-result:
		t.Fatalf("WebSocket write returned before cancellation: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled write = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		_ = peer.Close()
		<-result
		t.Fatal("cancellation did not unblock gorilla WebSocket writer")
	}
}

func TestWriteCodexWSMessageJoinsStartedCancellation(t *testing.T) {
	connection := newCodexWSJoinedCancellationConn()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- writeCodexWSMessage(ctx, connection, websocket.TextMessage, []byte("request"))
	}()
	<-connection.writeStarted
	cancel()
	<-connection.cancellationStarted
	select {
	case err := <-result:
		t.Fatalf("write returned before cancellation callback completed: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(connection.releaseCancellation)
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled write = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("write did not join cancellation callback")
	}
}

func TestWriteCodexWSMessagePreservesSuccessfulWriteAcrossConcurrentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	connection := &codexWSSuccessThenCancelConn{cancel: cancel}

	if err := writeCodexWSMessage(ctx, connection, websocket.TextMessage, []byte("response.completed")); err != nil {
		t.Fatalf("successful write across concurrent cancellation = %v, want nil", err)
	}
}

func TestReadCodexWSMessageJoinsStartedCancellation(t *testing.T) {
	serverConnection := make(chan *websocket.Conn, 1)
	releaseServer := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(writer, request, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		serverConnection <- connection
		<-releaseServer
		_ = connection.Close()
	}))
	t.Cleanup(func() {
		close(releaseServer)
		server.Close()
	})

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	<-serverConnection

	connection := &codexWSSignallingReadConn{
		Conn:                client,
		started:             make(chan struct{}),
		cancellationStarted: make(chan struct{}),
		releaseCancellation: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, _, err := readCodexWSMessage(ctx, connection)
		result <- err
	}()
	<-connection.started
	cancel()
	<-connection.cancellationStarted
	select {
	case err := <-result:
		t.Fatalf("read returned before cancellation callback completed: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(connection.releaseCancellation)
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled read = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("read did not join cancellation callback")
	}
}

type codexWSSignallingWriteConn struct {
	*websocket.Conn
	started chan struct{}
	once    sync.Once
}

func (connection *codexWSSignallingWriteConn) WriteMessage(messageType int, payload []byte) error {
	connection.once.Do(func() { close(connection.started) })
	return connection.Conn.WriteMessage(messageType, payload)
}

type codexWSSignallingReadConn struct {
	*websocket.Conn
	started             chan struct{}
	cancellationStarted chan struct{}
	releaseCancellation chan struct{}
	readOnce            sync.Once
	cancelOnce          sync.Once
}

func (connection *codexWSSignallingReadConn) ReadMessage() (int, []byte, error) {
	connection.readOnce.Do(func() { close(connection.started) })
	return connection.Conn.ReadMessage()
}

func (connection *codexWSSignallingReadConn) SetReadDeadline(deadline time.Time) error {
	err := connection.Conn.SetReadDeadline(deadline)
	connection.cancelOnce.Do(func() {
		close(connection.cancellationStarted)
		<-connection.releaseCancellation
	})
	return err
}

type codexWSJoinedCancellationConn struct {
	writeStarted        chan struct{}
	cancellationStarted chan struct{}
	releaseCancellation chan struct{}
	unblockWrite        chan struct{}
	cancelOnce          sync.Once
}

type codexWSSuccessThenCancelConn struct {
	cancel context.CancelFunc
}

func (*codexWSSuccessThenCancelConn) ReadMessage() (int, []byte, error) {
	return 0, nil, errors.New("unsupported")
}

func (connection *codexWSSuccessThenCancelConn) WriteMessage(int, []byte) error {
	connection.cancel()
	return nil
}

func (*codexWSSuccessThenCancelConn) WriteControl(int, []byte, time.Time) error { return nil }
func (*codexWSSuccessThenCancelConn) SetReadDeadline(time.Time) error           { return nil }
func (*codexWSSuccessThenCancelConn) SetWriteDeadline(time.Time) error          { return nil }
func (*codexWSSuccessThenCancelConn) Close() error                              { return nil }

func newCodexWSJoinedCancellationConn() *codexWSJoinedCancellationConn {
	return &codexWSJoinedCancellationConn{
		writeStarted:        make(chan struct{}),
		cancellationStarted: make(chan struct{}),
		releaseCancellation: make(chan struct{}),
		unblockWrite:        make(chan struct{}),
	}
}

func (connection *codexWSJoinedCancellationConn) cancelWrite() {
	connection.cancelOnce.Do(func() {
		close(connection.cancellationStarted)
		close(connection.unblockWrite)
		<-connection.releaseCancellation
	})
}

func (connection *codexWSJoinedCancellationConn) ReadMessage() (int, []byte, error) {
	return 0, nil, errors.New("unsupported")
}

func (connection *codexWSJoinedCancellationConn) WriteMessage(int, []byte) error {
	close(connection.writeStarted)
	<-connection.unblockWrite
	return errors.New("cancelled")
}

func (*codexWSJoinedCancellationConn) WriteControl(int, []byte, time.Time) error { return nil }
func (*codexWSJoinedCancellationConn) SetReadDeadline(time.Time) error           { return nil }
func (connection *codexWSJoinedCancellationConn) SetWriteDeadline(time.Time) error {
	connection.cancelWrite()
	return nil
}
func (connection *codexWSJoinedCancellationConn) Close() error {
	connection.cancelWrite()
	return nil
}
