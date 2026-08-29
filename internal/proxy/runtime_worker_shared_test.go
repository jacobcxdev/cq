package proxy

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestRuntimeProcessWorkerBootCancellationInterruptsPrivateSocket(t *testing.T) {
	supervisor, child := net.Pipe()
	defer child.Close()
	secret, err := NewRuntimeSecret(bytes.Repeat([]byte{0x51}, RuntimeSecretSize))
	if err != nil {
		t.Fatal(err)
	}
	worker := &runtimeProcessWorker{control: supervisor, secret: secret, receiver: NewRuntimeControlReceiver(secret), next: 1}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := worker.Boot(ctx, WorkerManifestV1{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Boot cancellation error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Boot cancellation took %v", elapsed)
	}
	if worker.control != nil {
		t.Fatal("cancelled private socket remained reusable")
	}
}

func runtimeHolder(description string) LifecycleHolderProof {
	return LifecycleHolderProof{
		LockIdentity:  fsutil.SecureFileIdentity{Device: 7, Inode: 11, Links: 1},
		DescriptionID: description,
		Mode:          LifecycleShared,
	}
}

func TestRuntimeWorkerRequiresDistinctHolderAndSelectedCheckpoint(t *testing.T) {
	calls := 0
	worker, err := NewRuntimeWorker(runtimeHolder("supervisor"), runtimeHolder("worker"), func(context.Context, RuntimeWorkV1) (RuntimeWorkResultV1, error) {
		calls++
		return RuntimeWorkResultV1{StatusCode: 204}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.Execute(context.Background(), RuntimeWorkV1{RequestID: "r-1"}); !errors.Is(err, ErrRuntimeCheckpointUnavailable) {
		t.Fatalf("pre-checkpoint Execute error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("handler calls = %d before checkpoint", calls)
	}
	if err := worker.SelectCheckpoint("checkpoint-digest"); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.Execute(context.Background(), RuntimeWorkV1{RequestID: "r-1"}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("handler calls = %d, want 1", calls)
	}
	if _, err := NewRuntimeWorker(runtimeHolder("same"), runtimeHolder("same"), nil); !errors.Is(err, ErrLifecycleHolderConflict) {
		t.Fatalf("duplicate holder error = %v", err)
	}
}

func TestRuntimeWorkerCancellationDoesNotInvokeHandler(t *testing.T) {
	worker, err := NewRuntimeWorker(runtimeHolder("supervisor"), runtimeHolder("worker"), func(context.Context, RuntimeWorkV1) (RuntimeWorkResultV1, error) {
		t.Fatal("handler invoked after cancellation")
		return RuntimeWorkResultV1{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.SelectCheckpoint("checkpoint-digest"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := worker.Execute(ctx, RuntimeWorkV1{RequestID: "r-1"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute error = %v", err)
	}
}
