//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package codex

import (
	"errors"
	"net"
	"net/rpc"
	"os"
	"path/filepath"
	"sync"
	"time"
)

func OpenCredentialControl(path string, coordinator *CredentialCoordinator) (*CredentialControl, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		if _, statErr := os.Lstat(path); statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				return nil, err
			}
			return nil, statErr
		}
		client, dialErr := dialCredentialOwner(path, 100*time.Millisecond)
		if dialErr != nil {
			return nil, dialErr
		}
		return &CredentialControl{client: client}, nil
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, err
	}
	if coordinator == nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, errors.New("credential coordinator unavailable")
	}
	server := rpc.NewServer()
	if err := server.RegisterName("CredentialRPC", &credentialRPC{Coordinator: coordinator}); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, err
	}
	var once sync.Once
	var connMu sync.Mutex
	var connWG sync.WaitGroup
	connections := make(map[net.Conn]struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			connMu.Lock()
			connections[conn] = struct{}{}
			connWG.Add(1)
			connMu.Unlock()
			go func() {
				defer connWG.Done()
				server.ServeConn(conn)
				_ = conn.Close()
				connMu.Lock()
				delete(connections, conn)
				connMu.Unlock()
			}()
		}
	}()
	closeOwner := func() error {
		var closeErr error
		once.Do(func() {
			closeErr = listener.Close()
			<-done
			connMu.Lock()
			for conn := range connections {
				_ = conn.Close()
			}
			connMu.Unlock()
			connWG.Wait()
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) && closeErr == nil {
				closeErr = err
			}
		})
		return closeErr
	}
	return &CredentialControl{owner: true, coordinator: coordinator, close: closeOwner}, nil
}

func dialCredentialOwner(path string, timeout time.Duration) (*rpc.Client, error) {
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, ErrCredentialOwnerStale
		}
		conn, err := net.DialTimeout("unix", path, remaining)
		if err == nil {
			_ = conn.SetDeadline(deadline)
			client := rpc.NewClient(conn)
			if pingErr := client.Call("CredentialRPC.Ping", struct{}{}, &struct{}{}); pingErr == nil {
				_ = conn.SetDeadline(time.Time{})
				return client, nil
			}
			_ = client.Close()
		}
		if wait := min(2*time.Millisecond, time.Until(deadline)); wait > 0 {
			time.Sleep(wait)
		}
	}
}
