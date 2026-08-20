package proxy

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestRealClientValidationConsumesOneDispatchBeforeObservation(t *testing.T) {
	nowValue := time.Unix(1_700_000_000, 0).UTC()
	now := func() time.Time { return nowValue }
	path := filepath.Join(t.TempDir(), "rcv")
	key := []byte("01234567890123456789012345678901")
	store, err := OpenRealClientValidationStore(fsutil.OSFileSystem{}, path, key, now, bytes.NewReader(bytes.Repeat([]byte{0x41}, 64)))
	if err != nil {
		t.Fatal(err)
	}
	preparation := RealClientValidationPreparationV1{OperationID: "operation", ValidationRunID: "run", FinalRouteChoiceDigest: strings.Repeat("a", 64), RequestDigest: strings.Repeat("b", 64)}
	if err := store.Prepare(context.Background(), preparation); err != nil {
		t.Fatal(err)
	}
	grant, err := store.IssueGrant(context.Background(), "operation")
	if err != nil {
		t.Fatal(err)
	}
	consumption, err := store.ConsumeDispatch(context.Background(), grant, strings.Repeat("c", 64))
	if err != nil {
		t.Fatal(err)
	}
	if consumption.DispatchCommittedAt.Before(grant.IssuedAt) {
		t.Fatal("dispatch consumption predates grant")
	}
	if _, err := store.ConsumeDispatch(context.Background(), grant, strings.Repeat("c", 64)); !errors.Is(err, ErrRealClientValidationReplay) {
		t.Fatalf("duplicate consumption error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenRealClientValidationStore(fsutil.OSFileSystem{}, path, key, now, bytes.NewReader(bytes.Repeat([]byte{0x42}, 64)))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.ConsumeDispatch(context.Background(), grant, strings.Repeat("c", 64)); !errors.Is(err, ErrRealClientValidationReplay) {
		t.Fatalf("reopened duplicate consumption error = %v", err)
	}
	receipt, err := reopened.Complete(context.Background(), grant, RealClientValidationObservationV1{Outcome: RealClientValidationPassed, ResponseDigest: strings.Repeat("d", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.RequirePromotionReceipt("operation", receipt.Digest); err != nil {
		t.Fatal(err)
	}
}

func TestRealClientValidationQuarantinesIndeterminateOutcome(t *testing.T) {
	nowValue := time.Unix(1_700_000_000, 0).UTC()
	store, err := OpenRealClientValidationStore(fsutil.OSFileSystem{}, filepath.Join(t.TempDir(), "rcv"), []byte("01234567890123456789012345678901"), func() time.Time { return nowValue }, bytes.NewReader(bytes.Repeat([]byte{0x43}, 64)))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	preparation := RealClientValidationPreparationV1{OperationID: "operation", ValidationRunID: "run", FinalRouteChoiceDigest: strings.Repeat("a", 64), RequestDigest: strings.Repeat("b", 64)}
	if err := store.Prepare(context.Background(), preparation); err != nil {
		t.Fatal(err)
	}
	grant, err := store.IssueGrant(context.Background(), "operation")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConsumeDispatch(context.Background(), grant, strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	receipt, err := store.Complete(context.Background(), grant, RealClientValidationObservationV1{Outcome: RealClientValidationIndeterminate})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RequirePromotionReceipt("operation", receipt.Digest); !errors.Is(err, ErrRealClientValidationQuarantined) {
		t.Fatalf("promotion error = %v", err)
	}
}
