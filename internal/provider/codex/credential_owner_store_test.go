package codex

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
)

type memoryCredentialAuthorityEntry struct {
	body     []byte
	identity CredentialAuthorityIdentity
}

type memoryCredentialAuthorityBackend struct {
	mu             *sync.Mutex
	gate           chan struct{}
	next           *uint64
	entries        map[string]memoryCredentialAuthorityEntry
	acquireStarted chan struct{}
}

func newMemoryCredentialAuthorityBackend() *memoryCredentialAuthorityBackend {
	var next uint64
	backend := &memoryCredentialAuthorityBackend{mu: &sync.Mutex{}, gate: make(chan struct{}, 1), next: &next, entries: make(map[string]memoryCredentialAuthorityEntry)}
	backend.gate <- struct{}{}
	return backend
}

func newMemoryCredentialAuthorityBackendSharing(storage *memoryCredentialAuthorityBackend) *memoryCredentialAuthorityBackend {
	backend := &memoryCredentialAuthorityBackend{mu: storage.mu, gate: make(chan struct{}, 1), next: storage.next, entries: storage.entries}
	backend.gate <- struct{}{}
	return backend
}

func (b *memoryCredentialAuthorityBackend) Acquire(ctx context.Context) (func() error, error) {
	if b.acquireStarted != nil {
		b.acquireStarted <- struct{}{}
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.gate:
	}
	var once sync.Once
	return func() error {
		once.Do(func() { b.gate <- struct{}{} })
		return nil
	}, nil
}

func (b *memoryCredentialAuthorityBackend) PublishImmutable(_ context.Context, name string, body []byte) (CredentialAuthorityIdentity, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.entries[name]; exists {
		return CredentialAuthorityIdentity{}, os.ErrExist
	}
	*b.next++
	identity := CredentialAuthorityIdentity{Device: 1, Inode: *b.next, Links: 1, Size: int64(len(body)), Digest: framedSHA256("test/identity/v1\x00", body)}
	b.entries[name] = memoryCredentialAuthorityEntry{body: append([]byte(nil), body...), identity: identity}
	return identity, nil
}

func (b *memoryCredentialAuthorityBackend) ReplaceSelectorExactPrior(_ context.Context, name string, prior CredentialAuthorityIdentity, body []byte) (CredentialAuthorityIdentity, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, exists := b.entries[name]
	if !exists || entry.identity != prior {
		return CredentialAuthorityIdentity{}, errors.New("prior mismatch")
	}
	*b.next++
	identity := CredentialAuthorityIdentity{Device: 1, Inode: *b.next, Links: 1, Size: int64(len(body)), Digest: framedSHA256("test/identity/v1\x00", body)}
	b.entries[name] = memoryCredentialAuthorityEntry{body: append([]byte(nil), body...), identity: identity}
	return identity, nil
}

func (b *memoryCredentialAuthorityBackend) Read(_ context.Context, name string, maxBytes int64) ([]byte, CredentialAuthorityIdentity, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, exists := b.entries[name]
	if !exists {
		return nil, CredentialAuthorityIdentity{}, os.ErrNotExist
	}
	if int64(len(entry.body)) > maxBytes {
		return nil, CredentialAuthorityIdentity{}, errors.New("object exceeds bound")
	}
	return append([]byte(nil), entry.body...), entry.identity, nil
}

func (b *memoryCredentialAuthorityBackend) CredentialAuthorityOccupancy(context.Context) (CredentialAuthorityOccupancy, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var bytes int64
	for _, entry := range b.entries {
		bytes += int64(len(entry.body))
	}
	return CredentialAuthorityOccupancy{Files: len(b.entries), Bytes: bytes, Units: len(b.entries)}, nil
}

func persistTestRefreshSelection(t *testing.T, backend CredentialAuthorityBackend, operationID string) RefreshMutationSelection {
	t.Helper()
	leaseBody, err := json.Marshal(refreshCapacityLeaseV1{
		SchemaVersion: 1, OperationID: operationID, Capacity: fullRefreshMutationCapacity(),
		Reserved: CredentialAuthorityOccupancy{Files: 573, Bytes: 12656640, Units: 3825},
	})
	if err != nil {
		t.Fatal(err)
	}
	leaseDigest := framedSHA256("cq/credential-owner/refresh-mutation/capacity-lease/v1\x00", leaseBody)
	if _, err := backend.PublishImmutable(context.Background(), "refresh-capacity-lease-"+leaseDigest+".json", leaseBody); err != nil {
		t.Fatal(err)
	}
	reservationBody, err := json.Marshal(refreshReservationV1{1, operationID, CandidateRef{AccountKey: "account", CandidateID: "candidate"}, "revision", leaseDigest})
	if err != nil {
		t.Fatal(err)
	}
	reservationDigest := framedSHA256("cq/credential-owner/refresh-mutation/reservation/v1\x00", reservationBody)
	if _, err := backend.PublishImmutable(context.Background(), "refresh-reservation-"+reservationDigest+".json", reservationBody); err != nil {
		t.Fatal(err)
	}
	return RefreshMutationSelection{ReservationDigest: reservationDigest, CapacityLeaseDigest: leaseDigest}
}

func TestCredentialOwnerCommitReceiptOrdering(t *testing.T) {
	backend := newMemoryCredentialAuthorityBackend()
	store, err := OpenCredentialOwnerStore(context.Background(), backend, make([]byte, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishReceipt("op", "receipt"); err == nil {
		t.Fatal("receipt accepted before commit")
	}
	selection := persistTestRefreshSelection(t, backend, "op")
	if _, err := store.PublishCommit("op", selection.ReservationDigest, "foreign-capacity-lease"); err == nil {
		t.Fatal("commit accepted a capacity lease not bound by the selected reservation")
	}
	commitDigest, err := store.PublishCommit("op", selection.ReservationDigest, selection.CapacityLeaseDigest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishReceipt("op", commitDigest); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenCredentialOwnerStore(context.Background(), backend, make([]byte, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.PublishReceipt("op", commitDigest); err != nil {
		t.Fatalf("idempotent terminal reopen: %v", err)
	}
}

func TestCredentialOwnerCrashAfterReceiptRecoversOriginalCommit(t *testing.T) {
	backend := newMemoryCredentialAuthorityBackend()
	crash := errors.New("crash")
	store, err := OpenCredentialOwnerStore(context.Background(), backend, make([]byte, 32), func(phase string) error {
		if phase == "receipt_durable" {
			return crash
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	selection := persistTestRefreshSelection(t, backend, "op")
	commitDigest, err := store.PublishCommit("op", selection.ReservationDigest, selection.CapacityLeaseDigest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishReceipt("op", commitDigest); !errors.Is(err, crash) {
		t.Fatalf("receipt error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenCredentialOwnerStore(context.Background(), backend, make([]byte, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.PublishReceipt("op", commitDigest); err != nil {
		t.Fatalf("recover receipt: %v", err)
	}
}

func TestCredentialOwnerContinuationRecoveryRejectsMismatchBeforeAdoption(t *testing.T) {
	backend := newMemoryCredentialAuthorityBackend()
	key := bytes.Repeat([]byte{0x41}, 32)
	crash := errors.New("continuation crash")
	store, err := OpenCredentialOwnerStore(context.Background(), backend, key, func(phase string) error {
		if phase == "continuation_durable" {
			return crash
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	selection := persistTestRefreshSelection(t, backend, "op")
	if _, err := store.PublishCommit("op", selection.ReservationDigest, selection.CapacityLeaseDigest); !errors.Is(err, crash) {
		t.Fatalf("PublishCommit error = %v, want continuation crash", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenCredentialOwnerStore(context.Background(), backend, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	recovery := RefreshMutationRecovery{
		OperationID: "op",
		Ref:         CandidateRef{AccountKey: "account", CandidateID: "candidate"},
		Revision:    "wrong-revision",
		Selection:   selection,
	}
	if _, err := reopened.RecoverRefresh(recovery); err == nil {
		t.Fatal("mismatched continuation was adopted")
	}
	recovery.Revision = "revision"
	if recovered, err := reopened.RecoverRefresh(recovery); err != nil || recovered.CommitDigest == "" {
		t.Fatalf("exact continuation recovery = %+v, %v", recovered, err)
	}
}

func TestCredentialOwnerRefreshResultEnvelopeMaxAndPlusOne(t *testing.T) {
	const envelopeMax = 1 << 20
	maximumErrorBytes := maximumRefreshResultErrorBytes(t, "op", strings.Repeat("a", 64), envelopeMax)
	for _, test := range []struct {
		name    string
		size    int
		wantErr bool
	}{
		{name: "max", size: maximumErrorBytes},
		{name: "plus one", size: maximumErrorBytes + 1, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := newMemoryCredentialAuthorityBackend()
			store, err := OpenCredentialOwnerStore(context.Background(), backend, bytes.Repeat([]byte{0x41}, 32), nil)
			if err != nil {
				t.Fatal(err)
			}
			selection := persistTestRefreshSelection(t, backend, "op")
			commitDigest, err := store.PublishCommit("op", selection.ReservationDigest, selection.CapacityLeaseDigest)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.PublishRefreshAttempt("op", commitDigest); err != nil {
				t.Fatal(err)
			}
			result := CredentialOwnerRefreshResult{Error: strings.Repeat("x", test.size)}
			err = store.PublishRefreshResult("op", commitDigest, result)
			if test.wantErr {
				if err == nil {
					t.Fatal("oversized refresh result accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			body, _, err := backend.Read(context.Background(), store.resultName("op"), envelopeMax)
			if err != nil || len(body) > envelopeMax {
				t.Fatalf("bounded envelope read = %d bytes, %v", len(body), err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenCredentialOwnerStore(context.Background(), backend, bytes.Repeat([]byte{0x41}, 32), nil)
			if err != nil {
				t.Fatal(err)
			}
			recovery, err := reopened.RecoverRefresh(RefreshMutationRecovery{
				OperationID: "op",
				Ref:         CandidateRef{AccountKey: "account", CandidateID: "candidate"},
				Revision:    "revision",
				Selection:   selection,
			})
			if err != nil || recovery.Result == nil || recovery.Result.Error != result.Error {
				t.Fatalf("maximum refresh result recovery = %+v, %v", recovery, err)
			}
		})
	}
}

func maximumRefreshResultErrorBytes(t *testing.T, operationID, commitDigest string, envelopeMax int) int {
	t.Helper()
	fixed, err := json.Marshal(credentialOwnerEncryptedResultV1{SchemaVersion: 1, Kind: "refresh_result", OperationID: operationID, CommitDigest: commitDigest})
	if err != nil {
		t.Fatal(err)
	}
	fits := func(errorBytes int) bool {
		plaintext, err := json.Marshal(CredentialOwnerRefreshResult{Error: strings.Repeat("x", errorBytes)})
		if err != nil {
			t.Fatal(err)
		}
		size := len(fixed) + base64.RawURLEncoding.EncodedLen(12) + base64.RawURLEncoding.EncodedLen(len(plaintext)+16)
		return size <= envelopeMax
	}
	low, high := 0, envelopeMax
	for low < high {
		middle := low + (high-low+1)/2
		if fits(middle) {
			low = middle
		} else {
			high = middle - 1
		}
	}
	return low
}

func TestCredentialOwnerWaiterReopensAfterTerminalAndCannotReplay(t *testing.T) {
	backend := newMemoryCredentialAuthorityBackend()
	key := make([]byte, 32)
	first, err := OpenCredentialOwnerStore(context.Background(), backend, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	selectionA := persistTestRefreshSelection(t, backend, "A")
	commitA, err := first.PublishCommit("A", selectionA.ReservationDigest, selectionA.CapacityLeaseDigest)
	if err != nil {
		t.Fatal(err)
	}
	type opened struct {
		store *CredentialOwnerStore
		err   error
	}
	waiter := make(chan opened, 1)
	backend.acquireStarted = make(chan struct{}, 1)
	go func() {
		store, err := OpenCredentialOwnerStore(context.Background(), backend, key, nil)
		waiter <- opened{store, err}
	}()
	<-backend.acquireStarted
	select {
	case <-waiter:
		t.Fatal("waiter read selected authority before acquiring mutation gate")
	default:
	}
	if _, err := first.PublishReceipt("A", commitA); err != nil {
		t.Fatal(err)
	}
	second := <-waiter
	if second.err != nil {
		t.Fatal(second.err)
	}
	if _, err := second.store.PublishCommit("A", selectionA.ReservationDigest, selectionA.CapacityLeaseDigest); err == nil {
		t.Fatal("waiter replayed terminal operation A")
	}
	selectionB := persistTestRefreshSelection(t, backend, "B")
	commitB, err := second.store.PublishCommit("B", selectionB.ReservationDigest, selectionB.CapacityLeaseDigest)
	if err != nil {
		t.Fatalf("second operation rollover: %v", err)
	}
	if _, err := second.store.PublishReceipt("B", commitB); err != nil {
		t.Fatal(err)
	}
}
