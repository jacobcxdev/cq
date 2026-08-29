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
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCallerDispatchPermitIsDurableAndOneUse(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	now := func() time.Time { return time.Unix(100, 0).UTC() }
	path := filepath.Join(t.TempDir(), "permits")
	request := CallerDispatchPermitRequestV2{
		CallerAdmissionDigest: string(bytes.Repeat([]byte{'a'}, 64)),
		CallerDomain:          NormalCallerCodex, CallerSubjectID: "caller-safe-id",
		SessionDigest: string(bytes.Repeat([]byte{'b'}, 64)), PoolID: testPoolIDA, RoutingGeneration: 7,
		AllowedAccounts: []codex.AccountKey{"account-a"}, SelectedAccount: "account-a",
	}
	store, err := OpenCallerDispatchPermitStore(fsutil.OSFileSystem{}, path, key, now, bytes.NewReader(bytes.Repeat([]byte{0x41}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	permit, err := store.IssueAndConsume(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if permit.SchemaVersion != 2 || permit.PoolID != testPoolIDA || permit.Digest == "" || permit.SelectedAccount != "account-a" || permit.ValidUntil.Sub(permit.IssuedAt) != 5*time.Second {
		t.Fatalf("permit = %#v", permit)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenCallerDispatchPermitStore(fsutil.OSFileSystem{}, path, key, now, bytes.NewReader(bytes.Repeat([]byte{0x41}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.IssueAndConsume(context.Background(), request); !errors.Is(err, ErrCallerDispatchPermitReplayed) {
		t.Fatalf("replay error = %v", err)
	}
}

func TestCallerDispatchPermitRejectsAccountOutsideFrozenAuthority(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	store, err := OpenCallerDispatchPermitStore(fsutil.OSFileSystem{}, filepath.Join(t.TempDir(), "permits"), key, func() time.Time { return time.Unix(100, 0).UTC() }, bytes.NewReader(bytes.Repeat([]byte{0x42}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = store.IssueAndConsume(context.Background(), CallerDispatchPermitRequestV2{
		CallerAdmissionDigest: string(bytes.Repeat([]byte{'a'}, 64)), CallerDomain: NormalCallerCodex, CallerSubjectID: "caller-safe-id",
		SessionDigest: string(bytes.Repeat([]byte{'b'}, 64)), PoolID: testPoolIDA, RoutingGeneration: 7,
		AllowedAccounts: []codex.AccountKey{"account-a"}, SelectedAccount: "account-b",
	})
	if !errors.Is(err, ErrCallerDispatchPermitInvalid) {
		t.Fatalf("error = %v", err)
	}
}

func TestCallerDispatchPermitRejectsSchemaV1Record(t *testing.T) {
	permit := CallerDispatchPermitV2{
		SchemaVersion: 1, PermitID: strings.Repeat("a", 32),
		CallerAdmissionDigest: strings.Repeat("b", 64), CallerDomain: NormalCallerCodex, CallerSubjectID: "caller-safe-id",
		SessionDigest: strings.Repeat("c", 64), PoolID: testPoolIDA, RoutingGeneration: 7,
		AllowedAccounts: []codex.AccountKey{"account-a"}, SelectedAccount: "account-a",
		IssuedAt: time.Unix(100, 0).UTC(), ValidUntil: time.Unix(105, 0).UTC(),
	}
	if err := validateCallerDispatchPermit(permit); !errors.Is(err, ErrCallerDispatchPermitInvalid) {
		t.Fatalf("error = %v", err)
	}
}
