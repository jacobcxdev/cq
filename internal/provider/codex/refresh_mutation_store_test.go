package codex

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"testing"
)

func TestRefreshMutationCapacityExactAndPlusOne(t *testing.T) {
	exact := RefreshMutationCapacity{
		MutationChildren: 256, OAuthChildren: 64, Decisions: 64,
		OperatorSlots: 2921, CredentialFiles: 573, WireFrames: 331,
		TotalUnits: 3825, TotalBytes: 522141696,
		CredentialBytes: 12656640, OuterObjectBytes: 262144,
		FixedDeltaBytes: 32768, PlanDeltaBytes: 98304, ReauthDeltaBytes: 512,
		SelectedLeaseDeltaBytes: 62, TerminalLeaseDeltaBytes: 62,
		DecisionDeltaBytes: 256, OAuthDeltaBytes: 384, MutationDeltaBytes: 320,
		OuterBaseObjects: 32, OAuthResultObjects: 64,
	}
	if err := exact.Validate(); err != nil {
		t.Fatalf("exact capacity: %v", err)
	}
	under := exact
	under.OperatorSlots--
	under.TotalUnits--
	if err := under.Validate(); err == nil {
		t.Fatal("accepted coherent-looking under-reservation instead of exact lease")
	}
	incoherent := exact
	incoherent.TotalUnits--
	if err := incoherent.Validate(); err == nil {
		t.Fatal("accepted incoherent aggregate")
	}
	for name, mutate := range map[string]func(*RefreshMutationCapacity){
		"mutation":             func(c *RefreshMutationCapacity) { c.MutationChildren++ },
		"oauth":                func(c *RefreshMutationCapacity) { c.OAuthChildren++ },
		"decision":             func(c *RefreshMutationCapacity) { c.Decisions++ },
		"operator slot":        func(c *RefreshMutationCapacity) { c.OperatorSlots++ },
		"credential file":      func(c *RefreshMutationCapacity) { c.CredentialFiles++ },
		"wire frame":           func(c *RefreshMutationCapacity) { c.WireFrames++ },
		"unit":                 func(c *RefreshMutationCapacity) { c.TotalUnits++ },
		"byte":                 func(c *RefreshMutationCapacity) { c.TotalBytes++ },
		"credential byte":      func(c *RefreshMutationCapacity) { c.CredentialBytes++ },
		"outer object byte":    func(c *RefreshMutationCapacity) { c.OuterObjectBytes++ },
		"fixed delta":          func(c *RefreshMutationCapacity) { c.FixedDeltaBytes++ },
		"plan delta":           func(c *RefreshMutationCapacity) { c.PlanDeltaBytes++ },
		"reauth delta":         func(c *RefreshMutationCapacity) { c.ReauthDeltaBytes++ },
		"selected lease delta": func(c *RefreshMutationCapacity) { c.SelectedLeaseDeltaBytes++ },
		"terminal lease delta": func(c *RefreshMutationCapacity) { c.TerminalLeaseDeltaBytes++ },
		"decision delta":       func(c *RefreshMutationCapacity) { c.DecisionDeltaBytes++ },
		"OAuth delta":          func(c *RefreshMutationCapacity) { c.OAuthDeltaBytes++ },
		"mutation delta":       func(c *RefreshMutationCapacity) { c.MutationDeltaBytes++ },
		"outer base":           func(c *RefreshMutationCapacity) { c.OuterBaseObjects++ },
		"OAuth result":         func(c *RefreshMutationCapacity) { c.OAuthResultObjects++ },
		"progress":             func(c *RefreshMutationCapacity) { c.ControlProgress = true },
	} {
		t.Run(name, func(t *testing.T) {
			got := exact
			mutate(&got)
			if err := got.Validate(); err == nil {
				t.Fatal("accepted +1 capacity")
			}
		})
	}
}

func TestRefreshMutationDurableSelectionPrecedesEffectAndRecovers(t *testing.T) {
	backend := newMemoryCredentialAuthorityBackend()
	capacity := fullRefreshMutationCapacity()
	ref := CandidateRef{AccountKey: "account", CandidateID: "candidate"}
	crash := errors.New("crash after selected anchor")
	store, err := OpenRefreshMutationStore(context.Background(), backend, bytes.Repeat([]byte{0x51}, 32), func(phase string) error {
		if phase == "selected_anchor_durable" {
			return crash
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	effects := 0
	if _, err := store.SelectRefreshMutation("op", ref, "revision", capacity); !errors.Is(err, crash) {
		t.Fatalf("Select error = %v", err)
	}
	if effects != 0 {
		t.Fatal("effect ran before selected authority returned")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenRefreshMutationStore(context.Background(), backend, bytes.Repeat([]byte{0x51}, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := reopened.SelectRefreshMutation("op", ref, "revision", capacity)
	if err != nil {
		t.Fatalf("recover selection: %v", err)
	}
	if selection.ReservationDigest == "" || selection.CapacityLeaseDigest == "" {
		t.Fatal("selection omitted durable reservation or capacity lease digest")
	}
	effects++
	terminalDigest, err := reopened.CompleteRefreshMutation("op", "credential-receipt")
	if err != nil || terminalDigest == "" {
		t.Fatal(err)
	}
	terminal, err := OpenRefreshMutationStore(context.Background(), backend, bytes.Repeat([]byte{0x51}, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := terminal.CompleteRefreshMutation("op", "credential-receipt"); err != nil {
		t.Fatalf("terminal reopen: %v", err)
	}
	second, err := terminal.SelectRefreshMutation("op-2", ref, "revision-2", capacity)
	if err != nil {
		t.Fatalf("second refresh selection: %v", err)
	}
	if second.ReservationDigest == selection.ReservationDigest || second.CapacityLeaseDigest == selection.CapacityLeaseDigest {
		t.Fatal("second refresh reused first reservation or capacity lease")
	}
	if _, err := terminal.CompleteRefreshMutation("op-2", "credential-receipt-2"); err != nil {
		t.Fatalf("second refresh completion: %v", err)
	}
	if effects != 1 {
		t.Fatalf("effects = %d, want 1", effects)
	}
}

func TestRefreshMutationRefusesInsufficientStableOperationCapacityBeforePublication(t *testing.T) {
	const stableOperationBytes = 3604480
	tests := []struct {
		name string
		seed func(*memoryCredentialAuthorityBackend)
	}{
		{
			name: "567 files",
			seed: func(backend *memoryCredentialAuthorityBackend) {
				for index := 0; index < 567; index++ {
					if _, err := backend.PublishImmutable(context.Background(), fmt.Sprintf("occupied-%03d", index), []byte{0x01}); err != nil {
						t.Fatal(err)
					}
				}
			},
		},
		{
			name: "near byte cap",
			seed: func(backend *memoryCredentialAuthorityBackend) {
				size := fullRefreshMutationCapacity().CredentialBytes - stableOperationBytes + 1
				if _, err := backend.PublishImmutable(context.Background(), "occupied-bytes", bytes.Repeat([]byte{0x01}, int(size))); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := newMemoryCredentialAuthorityBackend()
			test.seed(backend)
			before, err := backend.CredentialAuthorityOccupancy(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			store, err := OpenRefreshMutationStore(context.Background(), backend, bytes.Repeat([]byte{0x52}, 32), nil)
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.SelectRefreshMutation("op", CandidateRef{AccountKey: "account", CandidateID: "candidate"}, "revision", fullRefreshMutationCapacity())
			if err == nil {
				t.Fatal("insufficient stable-operation capacity accepted a refresh reservation")
			}
			after, err := backend.CredentialAuthorityOccupancy(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if after != before {
				t.Fatalf("capacity refusal changed occupancy from %+v to %+v", before, after)
			}
		})
	}
}

func TestRefreshMutationCompleteStableOperationFitsCredentialShare(t *testing.T) {
	backend := newMemoryCredentialAuthorityBackend()
	for index := 0; index < 561; index++ {
		if _, err := backend.PublishImmutable(context.Background(), fmt.Sprintf("occupied-%03d", index), []byte{0x01}); err != nil {
			t.Fatal(err)
		}
	}
	mutation, err := OpenRefreshMutationStore(context.Background(), backend, bytes.Repeat([]byte{0x52}, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := OpenCredentialOwnerStore(context.Background(), newMemoryCredentialAuthorityBackendSharing(backend), bytes.Repeat([]byte{0x43}, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := mutation.SelectRefreshMutation("op", CandidateRef{AccountKey: "account", CandidateID: "candidate"}, "revision", fullRefreshMutationCapacity())
	if err != nil {
		t.Fatal(err)
	}
	commitDigest, err := owner.PublishCommit("op", selection.ReservationDigest, selection.CapacityLeaseDigest)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.PublishRefreshAttempt("op", commitDigest); err != nil {
		t.Fatal(err)
	}
	if err := owner.PublishRefreshResult("op", commitDigest, CredentialOwnerRefreshResult{Material: CredentialMaterial{AccessToken: "access", RefreshToken: "refresh"}, ExpiresIn: 3600}); err != nil {
		t.Fatal(err)
	}
	receiptDigest, err := owner.PublishReceipt("op", commitDigest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mutation.CompleteRefreshMutation("op", receiptDigest); err != nil {
		t.Fatal(err)
	}
	occupied, err := backend.CredentialAuthorityOccupancy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if occupied.Files != 573 || occupied.Bytes > 12656640 || occupied.Units > 3825 {
		t.Fatalf("final credential-share occupancy = %+v, want 573 files and no closed-limit excess", occupied)
	}
}

func TestRefreshMutationSourceActionMapIsSignedAndOneToOne(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(bytes.NewReader(bytes.Repeat([]byte{0x42}, 64)))
	if err != nil {
		t.Fatal(err)
	}
	entries := []RefreshCredentialMutationSourceActionMapEntryV1{
		{SchemaVersion: 1, SourceRowRefDigest: "row-a", CatalogueRowOrdinal: 1, EntrypointID: "entry-a", EntrypointSymbol: "A", ImplementationSymbol: "implA", CallChainDigest: "chain-a", SourceID: "source-a", WriterKind: "atomic_file", Provider: "codex", SourceKind: "codex_refresh_persistence", ActionKind: "replace", TargetSelectorKind: "replace_file", TargetSemanticsRule: "atomic_replace_from_catalogue_grammars_v1"},
		{SchemaVersion: 1, SourceRowRefDigest: "row-b", CatalogueRowOrdinal: 2, EntrypointID: "entry-b", EntrypointSymbol: "B", ImplementationSymbol: "implB", CallChainDigest: "chain-b", SourceID: "source-b", WriterKind: "file_delete", LegacyOperationShape: stringPointer("descriptor_unlink"), Provider: "codex", SourceKind: "codex_refresh_persistence", ActionKind: "file_delete", TargetSelectorKind: "delete_file", TargetSemanticsRule: "file_delete_from_catalogue_grammar_v1"},
		{SchemaVersion: 1, SourceRowRefDigest: "row-c", CatalogueRowOrdinal: 3, EntrypointID: "entry-c", EntrypointSymbol: "C", ImplementationSymbol: "implC", CallChainDigest: "chain-c", SourceID: "source-c", WriterKind: "file_delete", LegacyOperationShape: stringPointer("file_unlink"), Provider: "codex", SourceKind: "codex_refresh_persistence", ActionKind: "cache_generation_invalidate", TargetSelectorKind: "cache_generation", TargetSemanticsRule: "quota_cache_generation_from_callsite_v1"},
		{SchemaVersion: 1, SourceRowRefDigest: "row-d", CatalogueRowOrdinal: 4, EntrypointID: "entry-d", EntrypointSymbol: "D", ImplementationSymbol: "implD", CallChainDigest: "chain-d", SourceID: "source-d", WriterKind: "native_keychain_upsert", LegacyOperationShape: stringPointer("platform_update_or_add"), Provider: "claude", SourceKind: "automatic_claude_persistence", ActionKind: "native_keychain_upsert", TargetSelectorKind: "native_upsert", TargetSemanticsRule: "claude_platform_update_or_add_v1"},
		{SchemaVersion: 1, SourceRowRefDigest: "row-e", CatalogueRowOrdinal: 5, EntrypointID: "entry-e", EntrypointSymbol: "E", ImplementationSymbol: "implE", CallChainDigest: "chain-e", SourceID: "source-e", WriterKind: "native_keychain_upsert", LegacyOperationShape: stringPointer("cq_set_delete_retry_set"), Provider: "claude", SourceKind: "cq_store_persistence", ActionKind: "native_keychain_upsert", TargetSelectorKind: "native_upsert", TargetSemanticsRule: "claude_cq_set_delete_retry_set_v1"},
		{SchemaVersion: 1, SourceRowRefDigest: "row-f", CatalogueRowOrdinal: 6, EntrypointID: "entry-f", EntrypointSymbol: "F", ImplementationSymbol: "implF", CallChainDigest: "chain-f", SourceID: "source-f", WriterKind: "native_keychain_delete", LegacyOperationShape: stringPointer("platform_repeated_service_delete"), Provider: "claude", SourceKind: "oauth_reauth", ActionKind: "native_keychain_delete", TargetSelectorKind: "native_delete", TargetSemanticsRule: "claude_platform_repeated_delete_v1"},
		{SchemaVersion: 1, SourceRowRefDigest: "row-g", CatalogueRowOrdinal: 7, EntrypointID: "entry-g", EntrypointSymbol: "G", ImplementationSymbol: "implG", CallChainDigest: "chain-g", SourceID: "source-g", WriterKind: "native_keychain_delete", LegacyOperationShape: stringPointer("cq_manifest_selector_delete"), Provider: "claude", SourceKind: "oauth_reauth", ActionKind: "native_keychain_delete", TargetSelectorKind: "native_delete", TargetSemanticsRule: "claude_cq_manifest_selector_delete_v1"},
	}
	got, err := SignRefreshCredentialMutationSourceActionMap(privateKey, "baseline", "release", "catalogue", entries)
	if err != nil {
		t.Fatal(err)
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	fingerprint := ReleaseBuildAuthorityPublicKeyFingerprint(publicKey)
	if err := got.Verify(publicKey, fingerprint, "baseline", "release", "catalogue"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	foreignPublic, _, err := ed25519.GenerateKey(bytes.NewReader(bytes.Repeat([]byte{0x43}, 64)))
	if err != nil {
		t.Fatal(err)
	}
	if err := got.Verify(foreignPublic, ReleaseBuildAuthorityPublicKeyFingerprint(foreignPublic), "baseline", "release", "catalogue"); err == nil {
		t.Fatal("accepted self-key substitution against foreign authority pin")
	}
	tampered := got
	tampered.Entries = append([]RefreshCredentialMutationSourceActionMapEntryV1(nil), got.Entries...)
	tampered.Entries[0].ActionKind = "file_delete"
	if err := tampered.Verify(publicKey, fingerprint, "baseline", "release", "catalogue"); err == nil {
		t.Fatal("accepted tampered signed map")
	}
	if err := got.Verify(publicKey, fingerprint, "foreign", "release", "catalogue"); err == nil {
		t.Fatal("accepted foreign baseline commit pin")
	}
	if err := got.Verify(publicKey, fingerprint, "baseline", "foreign", "catalogue"); err == nil {
		t.Fatal("accepted foreign release commit pin")
	}
	if err := got.Verify(publicKey, fingerprint, "baseline", "release", "foreign"); err == nil {
		t.Fatal("accepted foreign writer catalogue pin")
	}
	duplicate := append(entries, entries[0])
	if _, err := SignRefreshCredentialMutationSourceActionMap(privateKey, "baseline", "release", "catalogue", duplicate); err == nil {
		t.Fatal("accepted duplicate source row")
	}
	if _, err := SignRefreshCredentialMutationSourceActionMap(privateKey, "baseline", "release", "catalogue", nil); err == nil {
		t.Fatal("accepted empty exact source-action set")
	}
	nonnullAtomic := append([]RefreshCredentialMutationSourceActionMapEntryV1(nil), entries...)
	nonnullAtomic[0].LegacyOperationShape = stringPointer("")
	if _, err := SignRefreshCredentialMutationSourceActionMap(privateKey, "baseline", "release", "catalogue", nonnullAtomic); err == nil {
		t.Fatal("accepted non-null legacy operation shape for atomic writer")
	}
}

func stringPointer(value string) *string { return &value }
