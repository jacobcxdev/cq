//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package codex

import (
	"context"
	"errors"
	"net"
	"net/rpc"
	"sync"
	"time"
)

const credentialEndpointProtocolVersion = 1

type CredentialEndpointPingArgs struct {
	ProtocolVersion int
}

type CredentialEndpointPingReply struct {
	ProtocolVersion int
	Generation      string
}

type credentialEndpointRPC struct {
	generation       string
	control          *CredentialControl
	endpoint         *credentialEndpoint
	finaliseVerifier LegacyMaintenanceFinaliseVerifier
}

func (r *credentialEndpointRPC) Ping(args CredentialEndpointPingArgs, reply *CredentialEndpointPingReply) error {
	if args.ProtocolVersion != credentialEndpointProtocolVersion {
		return ErrCredentialEndpointIncompatible
	}
	reply.ProtocolVersion = credentialEndpointProtocolVersion
	reply.Generation = r.generation
	return nil
}

func OpenCredentialControl(path string, coordinator *CredentialCoordinator) (*CredentialControl, error) {
	return OpenCredentialControlPrepared(context.Background(), path, coordinator, nil)
}

// OpenCredentialControlPrepared opens the ordinary fail-closed endpoint and
// initializes a newly created owner before any coordinator RPC is accepted.
// Delegates never run the initializer.
func OpenCredentialControlPrepared(ctx context.Context, path string, coordinator *CredentialCoordinator, initializer CredentialOwnerInitializer) (*CredentialControl, error) {
	return openCredentialControlPrepared(ctx, path, coordinator, false, nil, initializer, nil)
}

// OpenCredentialControlPreparedWithLegacyMaintenanceVerifier injects the
// runtime-specific verifier used only by an explicitly requested maintenance
// finalise RPC. Ordinary credential operations never invoke it.
func OpenCredentialControlPreparedWithLegacyMaintenanceVerifier(ctx context.Context, path string, coordinator *CredentialCoordinator, initializer CredentialOwnerInitializer, verifier LegacyMaintenanceFinaliseVerifier) (*CredentialControl, error) {
	return openCredentialControlPreparedWithLegacyMaintenanceVerifier(ctx, path, coordinator, false, nil, initializer, nil, verifier)
}

// OpenRecoveringCredentialControl is reserved for supervised owner startup.
// Ordinary request and command paths must use OpenCredentialControl so they
// never mutate an existing endpoint while trying to connect.
func OpenRecoveringCredentialControl(path string, coordinator *CredentialCoordinator) (*CredentialControl, error) {
	return OpenRecoveringCredentialControlPrepared(context.Background(), path, coordinator, nil)
}

// OpenRecoveringCredentialControlPrepared is reserved for supervised owner
// startup. It may recover an exactly proved endpoint, then initializes the new
// owner before accepting coordinator RPCs. Delegates skip the initializer.
func OpenRecoveringCredentialControlPrepared(ctx context.Context, path string, coordinator *CredentialCoordinator, initializer CredentialOwnerInitializer) (*CredentialControl, error) {
	return openCredentialControlPrepared(ctx, path, coordinator, true, nil, initializer, nil)
}

// OpenRecoveringCredentialControlPreparedWithLegacyMaintenanceVerifier is the
// supervised recovery variant of OpenCredentialControlPreparedWithLegacyMaintenanceVerifier.
func OpenRecoveringCredentialControlPreparedWithLegacyMaintenanceVerifier(ctx context.Context, path string, coordinator *CredentialCoordinator, initializer CredentialOwnerInitializer, verifier LegacyMaintenanceFinaliseVerifier) (*CredentialControl, error) {
	return openCredentialControlPreparedWithLegacyMaintenanceVerifier(ctx, path, coordinator, true, nil, initializer, nil, verifier)
}

// OpenRecoveringCredentialControlPreparedWithLegacyMaintenanceVerifierAndRecoveryRecorder
// is the supervised recovery variant whose exact crash-recovery mutation is
// gated by a privacy-safe recorder.
func OpenRecoveringCredentialControlPreparedWithLegacyMaintenanceVerifierAndRecoveryRecorder(ctx context.Context, path string, coordinator *CredentialCoordinator, initializer CredentialOwnerInitializer, verifier LegacyMaintenanceFinaliseVerifier, recorder CredentialEndpointRecoveryRecorder) (*CredentialControl, error) {
	return openCredentialControlPreparedWithLegacyMaintenanceVerifierAndRecoveryObservation(ctx, path, coordinator, true, nil, initializer, nil, verifier, recorder, true)
}

func openCredentialControl(path string, coordinator *CredentialCoordinator, allowRecovery bool, phaseHook credentialEndpointPhaseHook) (*CredentialControl, error) {
	return openCredentialControlPrepared(context.Background(), path, coordinator, allowRecovery, phaseHook, nil, nil)
}

func openCredentialControlPrepared(ctx context.Context, path string, coordinator *CredentialCoordinator, allowRecovery bool, phaseHook credentialEndpointPhaseHook, initializer CredentialOwnerInitializer, beforeAccept func()) (*CredentialControl, error) {
	return openCredentialControlPreparedWithLegacyMaintenanceVerifier(ctx, path, coordinator, allowRecovery, phaseHook, initializer, beforeAccept, nil)
}

func openCredentialControlPreparedWithLegacyMaintenanceVerifier(ctx context.Context, path string, coordinator *CredentialCoordinator, allowRecovery bool, phaseHook credentialEndpointPhaseHook, initializer CredentialOwnerInitializer, beforeAccept func(), verifier LegacyMaintenanceFinaliseVerifier) (*CredentialControl, error) {
	return openCredentialControlPreparedWithLegacyMaintenanceVerifierAndRecoveryObservation(ctx, path, coordinator, allowRecovery, phaseHook, initializer, beforeAccept, verifier, nil, false)
}

func openCredentialControlPreparedWithLegacyMaintenanceVerifierAndRecoveryObservation(ctx context.Context, path string, coordinator *CredentialCoordinator, allowRecovery bool, phaseHook credentialEndpointPhaseHook, initializer CredentialOwnerInitializer, beforeAccept func(), verifier LegacyMaintenanceFinaliseVerifier, recorder CredentialEndpointRecoveryRecorder, recoveryRecordRequired bool) (*CredentialControl, error) {
	var endpoint *credentialEndpoint
	var client *rpc.Client
	var err error
	if recoveryRecordRequired {
		endpoint, client, err = openCredentialEndpointWithRecoveryRecorderContext(ctx, path, allowRecovery, phaseHook, recorder)
	} else {
		endpoint, client, err = openCredentialEndpointWithContext(ctx, path, allowRecovery, phaseHook)
	}
	if err != nil {
		return nil, err
	}
	if client != nil {
		return &CredentialControl{client: client}, nil
	}
	if coordinator == nil {
		_ = endpoint.Close()
		return nil, errors.New("credential coordinator unavailable")
	}

	listener := endpoint.listener
	var once sync.Once
	var connMu sync.Mutex
	var connWG sync.WaitGroup
	connections := make(map[net.Conn]struct{})
	done := make(chan struct{})
	acceptStarted := false
	closeOwner := func() error {
		var closeErr error
		once.Do(func() {
			closeErr = listener.Close()
			if acceptStarted {
				<-done
				connMu.Lock()
				for conn := range connections {
					_ = conn.Close()
				}
				connMu.Unlock()
				connWG.Wait()
			}
			if err := endpoint.CloseAfterListener(); err != nil && closeErr == nil {
				closeErr = err
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
	if err := server.RegisterName("CredentialEndpoint", &credentialEndpointRPC{
		generation: endpoint.generation, control: control, endpoint: endpoint, finaliseVerifier: verifier,
	}); err != nil {
		return nil, errors.Join(err, control.Close())
	}
	if beforeAccept != nil {
		beforeAccept()
	}
	acceptStarted = true
	go func() {
		defer close(done)
		defer func() { _ = recover() }()
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
				defer func() {
					_ = recover()
					_ = conn.Close()
					connMu.Lock()
					delete(connections, conn)
					connMu.Unlock()
					connWG.Done()
				}()
				server.ServeConn(conn)
			}()
		}
	}()
	return control, nil
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
			var reply CredentialEndpointPingReply
			pingErr := client.Call("CredentialEndpoint.Ping", CredentialEndpointPingArgs{ProtocolVersion: credentialEndpointProtocolVersion}, &reply)
			if pingErr == nil && reply.ProtocolVersion == credentialEndpointProtocolVersion && reply.Generation != "" {
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
