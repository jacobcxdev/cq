//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package codex

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/rpc"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

type stalledMaintenanceRPC struct {
	pings     atomic.Int32
	finalises atomic.Int32
	stall     string
	release   <-chan struct{}
}

func (r *stalledMaintenanceRPC) Ping(_ CredentialEndpointPingArgs, reply *CredentialEndpointPingReply) error {
	n := r.pings.Add(1)
	if r.stall == "first-ping" || r.stall == "second-ping" && n == 2 {
		<-r.release
	}
	reply.ProtocolVersion = credentialEndpointProtocolVersion
	reply.Generation = strings.Repeat("a", 32)
	return nil
}
func (r *stalledMaintenanceRPC) FinaliseMaintenance(_ LegacyCredentialEndpointFinaliseRPCArgs, _ *LegacyCredentialEndpointFinaliseRPCReply) error {
	r.finalises.Add(1)
	<-r.release
	return ErrCredentialEndpointMaintenanceVerifierRequired
}

func TestCredentialEndpointMaintenanceFinaliseBudget(t *testing.T) {
	for _, stage := range []string{"first-ping", "second-ping", "finalise", "cancel-after-ping"} {
		t.Run(stage, func(t *testing.T) {
			path := createRefusedLegacyCredentialSocket(t)
			ctx := context.Background()
			snapshot, err := InspectLegacyCredentialEndpoint(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			transition, err := PrepareLegacyCredentialEndpointTransition(ctx, path, snapshot, DrainAuthorityFunc(func(context.Context, string) error { return nil }))
			if err != nil {
				t.Fatal(err)
			}
			ticket := transition.Ticket()
			if err := transition.Activate(ctx); err != nil {
				t.Fatal(err)
			}
			if err := transition.Close(); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			release := make(chan struct{})
			rpcImpl := &stalledMaintenanceRPC{stall: stage, release: release}
			if stage == "cancel-after-ping" {
				rpcImpl.stall = "second-ping"
			}
			server := rpc.NewServer()
			if err := server.RegisterName("CredentialEndpoint", rpcImpl); err != nil {
				t.Fatal(err)
			}
			served := make(chan struct{})
			go func() {
				defer close(served)
				conn, err := listener.Accept()
				if err == nil {
					defer conn.Close()
					server.ServeConn(conn)
				}
			}()
			timer := time.AfterFunc(180*time.Millisecond, func() { close(release) })
			defer func() {
				if timer.Stop() {
					close(release)
				}
				listener.Close()
				<-served
			}()
			before := maintenanceDirectoryInventory(t, filepath.Dir(path))
			budget, cancel := context.WithTimeout(ctx, 35*time.Millisecond)
			expected := context.DeadlineExceeded
			if stage == "cancel-after-ping" {
				cancel()
				budget, cancel = context.WithCancel(ctx)
				stop := time.AfterFunc(35*time.Millisecond, cancel)
				defer stop.Stop()
				expected = context.Canceled
			}
			defer cancel()
			started := time.Now()
			err = FinaliseLegacyCredentialEndpointTransition(budget, path, ticket)
			if !errors.Is(err, expected) {
				t.Errorf("error=%v want %v", err, expected)
			}
			if elapsed := time.Since(started); elapsed > 130*time.Millisecond {
				t.Errorf("deadline exceeded: %s", elapsed)
			}
			if stage != "finalise" && rpcImpl.finalises.Load() != 0 {
				t.Error("sent finalise after stalled ping")
			}
			if rpcImpl.finalises.Load() > 1 {
				t.Error("replayed finalise")
			}
			if after := maintenanceDirectoryInventory(t, filepath.Dir(path)); !reflect.DeepEqual(before, after) {
				t.Error("unproved health changed journal inventory")
			}
		})
	}
}

func TestCredentialEndpointMaintenanceFinaliseDisconnectIndeterminate(t *testing.T) {
	path := createRefusedLegacyCredentialSocket(t)
	ctx := context.Background()
	snapshot, err := InspectLegacyCredentialEndpoint(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	transition, err := PrepareLegacyCredentialEndpointTransition(ctx, path, snapshot, DrainAuthorityFunc(func(context.Context, string) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	ticket := transition.Ticket()
	if err := transition.Activate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := transition.Close(); err != nil {
		t.Fatal(err)
	}
	coordinator, _ := testCoordinator(t)
	release := make(chan struct{})
	entered := make(chan struct{})
	var acquired atomic.Int32
	verifier := testLegacyMaintenanceFinaliseVerifier(func(context.Context, LegacyMaintenanceFinaliseVerification) error {
		if acquired.Add(1) == 1 {
			close(entered)
		}
		<-release
		return nil
	})
	owner, err := OpenCredentialControlPreparedWithLegacyMaintenanceVerifier(ctx, path, coordinator, nil, verifier)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	budget, cancel := context.WithTimeout(ctx, 60*time.Millisecond)
	defer cancel()
	if err := FinaliseLegacyCredentialEndpointTransition(budget, path, ticket); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	select {
	case <-entered:
	default:
		t.Fatal("request did not reach live health verifier")
	}
	status, err := InspectLegacyCredentialEndpointTransition(ctx, path)
	if err != nil || status.State != CredentialEndpointMaintenanceActivated {
		t.Fatalf("unproved state=%s err=%v", status.State, err)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(path), ticket.QuarantineName)); err != nil {
		t.Fatal("unproved health retired rollback")
	}
	close(release)
	released = true
	deadline := time.Now().Add(time.Second)
	for {
		_, err := os.Lstat(credentialEndpointMaintenanceRollbackPath(path))
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("accepted verified request did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	if acquired.Load() != 1 {
		t.Fatal("timeout replayed submitted finalise")
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(path), ticket.QuarantineName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("verified completion retained quarantine")
	}
	// Only a fresh, explicit invocation may reconcile a previously unknown result.
	if err := FinaliseLegacyCredentialEndpointTransition(ctx, path, ticket); err != nil {
		t.Fatal(err)
	}
	if acquired.Load() != 1 {
		t.Fatal("committed ticket replay reacquired health authority")
	}
}

func TestCredentialEndpointMaintenanceRollbackBudget(t *testing.T) {
	for _, cancellation := range []bool{false, true} {
		t.Run(fmt.Sprint(cancellation), func(t *testing.T) {
			path, ticket, owner := openActivatedMaintenanceEndpointForFinaliseTest(t)
			// Retain the exact published socket and owner receipt but release its lock.
			// No RPC service consumes Ping, so the authenticated stale-owner probe stalls.
			owner.release()
			ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
			want := context.DeadlineExceeded
			if cancellation {
				cancel()
				ctx, cancel = context.WithCancel(context.Background())
				timer := time.AfterFunc(35*time.Millisecond, cancel)
				defer timer.Stop()
				want = context.Canceled
			}
			defer cancel()
			drain := DrainAuthorityFunc(func(context.Context, string) error { return nil })
			transition, err := ResumeLegacyCredentialEndpointTransitionForAction(ctx, path, ticket, drain, LegacyCredentialEndpointRollback)
			if err != nil {
				t.Fatal(err)
			}
			defer transition.Close()
			started := time.Now()
			err = transition.Rollback(ctx)
			if !errors.Is(err, want) {
				t.Errorf("error=%v want %v", err, want)
			}
			if elapsed := time.Since(started); elapsed > 130*time.Millisecond {
				t.Errorf("rollback exceeded deadline: %s", elapsed)
			}
			for _, retained := range []string{path, credentialEndpointSidecarPath(path), credentialEndpointMaintenanceRollbackPath(path), filepath.Join(filepath.Dir(path), ticket.QuarantineName)} {
				if _, err := os.Lstat(retained); err != nil {
					t.Errorf("rollback probe removed %s: %v", filepath.Base(retained), err)
				}
			}
		})
	}
}

func maintenanceExactInventory(t *testing.T, path string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]string{}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		id, ok := (fsutil.OSFileSystem{}).FileIdentity(info)
		if !ok {
			t.Fatal("missing identity")
		}
		value := fmt.Sprintf("%v/%v/%v/%v", id, info.Mode(), info.Size(), info.ModTime())
		if info.Mode().IsRegular() {
			body, err := os.ReadFile(filepath.Join(filepath.Dir(path), entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			value += "/" + string(body)
		}
		result[entry.Name()] = value
	}
	return result
}

func TestCredentialEndpointMaintenanceReopenStableAndInterrupted(t *testing.T) {
	for _, phase := range []CredentialEndpointMaintenanceState{CredentialEndpointMaintenancePrepared, CredentialEndpointMaintenanceQuarantined, CredentialEndpointMaintenanceActivating, CredentialEndpointMaintenanceActivated, CredentialEndpointMaintenanceFinalising, CredentialEndpointMaintenanceCommitting, CredentialEndpointMaintenanceRollingBack, CredentialEndpointMaintenanceRolledBack} {
		t.Run(string(phase), func(t *testing.T) {
			path := createRefusedLegacyCredentialSocket(t)
			ctx := context.Background()
			snapshot, err := InspectLegacyCredentialEndpoint(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			drain := DrainAuthorityFunc(func(context.Context, string) error { return nil })
			transition, err := PrepareLegacyCredentialEndpointTransition(ctx, path, snapshot, drain)
			if err != nil {
				t.Fatal(err)
			}
			ticket := transition.Ticket()
			switch phase {
			case CredentialEndpointMaintenanceActivated, CredentialEndpointMaintenanceFinalising:
				if err := transition.Activate(ctx); err != nil {
					t.Fatal(err)
				}
			case CredentialEndpointMaintenanceRolledBack:
				if err := transition.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if err := transition.Close(); err != nil {
				t.Fatal(err)
			}
			var owner *credentialEndpoint
			if phase == CredentialEndpointMaintenanceFinalising {
				var client *rpc.Client
				owner, client, err = openCredentialEndpoint(path, false, nil)
				if err != nil || client != nil || owner == nil {
					t.Fatalf("candidate endpoint: owner=%v client=%v err=%v", owner, client, err)
				}
				defer owner.Close()
			}
			stable := phase == CredentialEndpointMaintenanceQuarantined || phase == CredentialEndpointMaintenanceActivated || phase == CredentialEndpointMaintenanceRolledBack
			if !stable {
				file := credentialEndpointMaintenanceJournalPath(path)
				if phase == CredentialEndpointMaintenanceFinalising {
					file = credentialEndpointMaintenanceRollbackPath(path)
				}
				body, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				record, err := decodeCredentialEndpointMaintenanceJournal(body)
				if err != nil {
					t.Fatal(err)
				}
				record.State = phase
				record.Generation++
				body, err = encodeCredentialEndpointMaintenanceJournal(record)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, body, 0o600); err != nil {
					t.Fatal(err)
				}
				if owner != nil {
					if err := owner.listener.Close(); err != nil {
						t.Fatal(err)
					}
					owner.release()
				}
			}
			before := maintenanceExactInventory(t, path)
			for i := 0; i < 2; i++ {
				status, err := ReopenLegacyCredentialEndpointTransition(ctx, path, ticket, drain)
				if stable {
					if err != nil || status.State != phase || status.Ticket != ticket {
						t.Fatalf("state=%s err=%v", status.State, err)
					}
				} else if err == nil {
					t.Fatal("interrupted resume succeeded")
				}
			}
			if after := maintenanceExactInventory(t, path); !reflect.DeepEqual(before, after) {
				t.Fatal("resume changed bytes or identities")
			}
			if !stable && phase != CredentialEndpointMaintenancePrepared && phase != CredentialEndpointMaintenanceActivating {
				opened, err := ResumeLegacyCredentialEndpointTransitionForAction(ctx, path, ticket, drain, LegacyCredentialEndpointActivate)
				if opened != nil {
					opened.Close()
				}
				if err == nil {
					t.Fatal("activate resumed unrelated phase")
				}
				if after := maintenanceExactInventory(t, path); !reflect.DeepEqual(before, after) {
					t.Fatal("rejected activate changed unrelated phase")
				}
			}
			if phase == CredentialEndpointMaintenanceActivating || phase == CredentialEndpointMaintenanceCommitting || phase == CredentialEndpointMaintenanceFinalising {
				opened, err := ResumeLegacyCredentialEndpointTransitionForAction(ctx, path, ticket, drain, LegacyCredentialEndpointRollback)
				if opened != nil {
					opened.Close()
				}
				if err == nil {
					t.Fatal("rollback resumed unrelated phase")
				}
				if after := maintenanceExactInventory(t, path); !reflect.DeepEqual(before, after) {
					t.Fatal("rejected rollback changed unrelated phase")
				}
			}
		})
	}
}

func TestCredentialEndpointMaintenanceReopenAuthority(t *testing.T) {
	for _, kind := range []string{"ticket", "directory", "socket", "lock", "drain", "cancel", "deadline", "lock-held", "lock-missing"} {
		t.Run(kind, func(t *testing.T) {
			path := createRefusedLegacyCredentialSocket(t)
			ctx := context.Background()
			snapshot, err := InspectLegacyCredentialEndpoint(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			drain := DrainAuthorityFunc(func(context.Context, string) error { return nil })
			transition, err := PrepareLegacyCredentialEndpointTransition(ctx, path, snapshot, drain)
			if err != nil {
				t.Fatal(err)
			}
			ticket := transition.Ticket()
			if kind != "lock-held" {
				if err := transition.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				defer transition.Close()
			}
			switch kind {
			case "ticket":
				ticket.ID = strings.Repeat("b", 32)
			case "directory":
				ticket.Directory.Inode++
			case "socket":
				ticket.Socket.Inode++
			case "lock":
				ticket.Lock.Inode++
			case "drain":
				drain = func(context.Context, string) error { return ErrCredentialEndpointMaintenanceDrainRequired }
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "deadline":
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
			case "lock-missing":
				if err := os.Remove(credentialEndpointLockPath(path)); err != nil {
					t.Fatal(err)
				}
			}
			before := maintenanceExactInventory(t, path)
			if _, err := ReopenLegacyCredentialEndpointTransition(ctx, path, ticket, drain); err == nil {
				t.Fatal("invalid authority accepted")
			}
			if after := maintenanceExactInventory(t, path); !reflect.DeepEqual(before, after) {
				t.Fatal("reopen repaired invalid authority")
			}
		})
	}
}
