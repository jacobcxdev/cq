//go:build windows

package codex

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/rpc"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

const (
	credentialEndpointProtocolVersion = 1
	windowsCredentialDialTimeout      = 250 * time.Millisecond
	windowsCredentialRaceTimeout      = 2 * time.Second
)

var ErrCredentialEndpointIncompatible = errors.New("credential coordinator endpoint protocol incompatible")

type CredentialEndpointPingArgs struct {
	ProtocolVersion int
}

type CredentialEndpointPingReply struct {
	ProtocolVersion int
	Generation      string
}

type windowsCredentialEndpointRPC struct {
	generation string
}

func (endpoint *windowsCredentialEndpointRPC) Ping(args CredentialEndpointPingArgs, reply *CredentialEndpointPingReply) error {
	if endpoint == nil || reply == nil || args.ProtocolVersion != credentialEndpointProtocolVersion {
		return ErrCredentialEndpointIncompatible
	}
	reply.ProtocolVersion = credentialEndpointProtocolVersion
	reply.Generation = endpoint.generation
	return nil
}

func OpenCredentialControl(path string, coordinator *CredentialCoordinator) (*CredentialControl, error) {
	return OpenCredentialControlPrepared(context.Background(), path, coordinator, nil)
}

func OpenCredentialControlPrepared(ctx context.Context, path string, coordinator *CredentialCoordinator, initializer CredentialOwnerInitializer) (*CredentialControl, error) {
	return openWindowsCredentialControlPrepared(ctx, path, coordinator, initializer)
}

func OpenCredentialControlPreparedWithLegacyMaintenanceVerifier(ctx context.Context, path string, coordinator *CredentialCoordinator, initializer CredentialOwnerInitializer, _ LegacyMaintenanceFinaliseVerifier) (*CredentialControl, error) {
	return openWindowsCredentialControlPrepared(ctx, path, coordinator, initializer)
}

func OpenRecoveringCredentialControl(path string, coordinator *CredentialCoordinator) (*CredentialControl, error) {
	return OpenRecoveringCredentialControlPrepared(context.Background(), path, coordinator, nil)
}

func OpenRecoveringCredentialControlPrepared(ctx context.Context, path string, coordinator *CredentialCoordinator, initializer CredentialOwnerInitializer) (*CredentialControl, error) {
	return openWindowsCredentialControlPrepared(ctx, path, coordinator, initializer)
}

func OpenRecoveringCredentialControlPreparedWithLegacyMaintenanceVerifier(ctx context.Context, path string, coordinator *CredentialCoordinator, initializer CredentialOwnerInitializer, _ LegacyMaintenanceFinaliseVerifier) (*CredentialControl, error) {
	return openWindowsCredentialControlPrepared(ctx, path, coordinator, initializer)
}

func OpenRecoveringCredentialControlPreparedWithLegacyMaintenanceVerifierAndRecoveryRecorder(ctx context.Context, path string, coordinator *CredentialCoordinator, initializer CredentialOwnerInitializer, _ LegacyMaintenanceFinaliseVerifier, _ CredentialEndpointRecoveryRecorder) (*CredentialControl, error) {
	return openWindowsCredentialControlPrepared(ctx, path, coordinator, initializer)
}

func openWindowsCredentialControlPrepared(ctx context.Context, path string, coordinator *CredentialCoordinator, initializer CredentialOwnerInitializer) (*CredentialControl, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	pipePath, sid, err := windowsCredentialPipePath(path)
	if err != nil {
		return nil, err
	}
	if client, err := dialWindowsCredentialOwner(ctx, pipePath, windowsCredentialDialTimeout); err == nil {
		return &CredentialControl{client: client}, nil
	}
	listener, err := winio.ListenPipe(pipePath, &winio.PipeConfig{
		SecurityDescriptor: windowsCredentialPipeSDDL(sid),
		InputBufferSize:    64 << 10,
		OutputBufferSize:   64 << 10,
	})
	if err != nil {
		client, dialErr := dialWindowsCredentialOwner(ctx, pipePath, windowsCredentialRaceTimeout)
		if dialErr == nil {
			return &CredentialControl{client: client}, nil
		}
		return nil, ErrCredentialOwnerStale
	}
	return serveWindowsCredentialOwner(ctx, listener, coordinator, initializer)
}

func serveWindowsCredentialOwner(ctx context.Context, listener net.Listener, coordinator *CredentialCoordinator, initializer CredentialOwnerInitializer) (*CredentialControl, error) {
	if coordinator == nil {
		return nil, errors.Join(errors.New("credential coordinator unavailable"), listener.Close())
	}
	var generationBytes [16]byte
	if _, err := rand.Read(generationBytes[:]); err != nil {
		return nil, errors.Join(err, listener.Close())
	}
	generation := hex.EncodeToString(generationBytes[:])

	var once sync.Once
	var connectionMu sync.Mutex
	var connectionWG sync.WaitGroup
	connections := make(map[net.Conn]struct{})
	done := make(chan struct{})
	acceptStarted := false
	closeOwner := func() error {
		var closeErr error
		once.Do(func() {
			closeErr = listener.Close()
			if acceptStarted {
				<-done
				connectionMu.Lock()
				for connection := range connections {
					_ = connection.Close()
				}
				connectionMu.Unlock()
				connectionWG.Wait()
			}
		})
		return closeErr
	}
	control := &CredentialControl{owner: true, coordinator: coordinator, close: closeOwner}
	if initializer != nil {
		initializerErr := func() error {
			defer func() {
				if recovered := recover(); recovered != nil {
					_ = control.Close()
					panic(recovered)
				}
			}()
			return initializer(ctx, coordinator, control)
		}()
		if initializerErr != nil {
			return nil, errors.Join(initializerErr, control.Close())
		}
	}

	server := rpc.NewServer()
	if err := server.RegisterName("CredentialRPC", &credentialRPC{Coordinator: coordinator, Control: control}); err != nil {
		return nil, errors.Join(err, control.Close())
	}
	if err := server.RegisterName("CredentialEndpoint", &windowsCredentialEndpointRPC{generation: generation}); err != nil {
		return nil, errors.Join(err, control.Close())
	}
	acceptStarted = true
	go func() {
		defer close(done)
		defer func() { _ = recover() }()
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			connectionMu.Lock()
			connections[connection] = struct{}{}
			connectionWG.Add(1)
			connectionMu.Unlock()
			go func() {
				defer func() {
					_ = recover()
					_ = connection.Close()
					connectionMu.Lock()
					delete(connections, connection)
					connectionMu.Unlock()
					connectionWG.Done()
				}()
				server.ServeConn(connection)
			}()
		}
	}()
	return control, nil
}

func dialWindowsCredentialOwner(parent context.Context, pipePath string, timeout time.Duration) (*rpc.Client, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	connection, err := winio.DialPipeContext(ctx, pipePath)
	if err != nil {
		return nil, err
	}
	client := rpc.NewClient(connection)
	var reply CredentialEndpointPingReply
	if err := client.Call("CredentialEndpoint.Ping", CredentialEndpointPingArgs{ProtocolVersion: credentialEndpointProtocolVersion}, &reply); err != nil {
		_ = client.Close()
		return nil, ErrCredentialOwnerStale
	}
	if reply.ProtocolVersion != credentialEndpointProtocolVersion || reply.Generation == "" {
		_ = client.Close()
		return nil, ErrCredentialEndpointIncompatible
	}
	return client, nil
}

func windowsCredentialPipePath(path string) (string, string, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", "", fmt.Errorf("invalid Windows credential coordinator path")
	}
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return "", "", fmt.Errorf("open Windows credential coordinator token: %w", err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return "", "", fmt.Errorf("read Windows credential coordinator SID")
	}
	sid := user.User.Sid.String()
	digest := sha256.Sum256([]byte("cq/credential-pipe/windows/v1\x00" + sid + "\x00" + strings.ToLower(path)))
	return `\\.\pipe\cq-credential-` + hex.EncodeToString(digest[:16]), sid, nil
}

func windowsCredentialPipeSDDL(sid string) string {
	return "O:" + sid + "G:" + sid + "D:P(A;;GA;;;" + sid + ")(A;;GA;;;SY)"
}
