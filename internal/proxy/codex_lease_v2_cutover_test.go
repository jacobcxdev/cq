package proxy

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexLeaseV2CompleteLegacyCutoverUsesPinnedHorizonAndCAS(t *testing.T) {
	fsys := fsutil.NewMemFS()
	now := time.Date(2026, 8, 9, 4, 5, 6, 700, time.UTC)
	retention := 48 * time.Hour
	key, v1Bytes := writeCodexLeaseV1Fixture(t, fsys)
	digest := codexLeaseSHA256(v1Bytes)
	owner := &cutoverTestOwner{}
	coordinator, err := OpenCodexContinuityCoordinator(CodexContinuityOpenOptions{
		FS:          fsys,
		KeyPath:     "/state/leases.key",
		JournalPath: "/state/leases.json",
		Policy: CodexLeasePolicy{
			Retention: retention,
			Now:       func() time.Time { return now },
		},
		Modes: CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{4, 6}},
	}, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()

	store := coordinator.Store()
	if got := store.Generation(); got != 8 {
		t.Fatalf("migration generation = %d, want 8", got)
	}
	quarantinedBytes := readCutoverTestFile(t, fsys, "/state/leases.json")
	horizon := now.Add(retention)

	now = horizon.Add(-time.Nanosecond)
	if generation, err := store.CompleteLegacyCutover(8, CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{4, 6}}); generation != 0 || !errors.Is(err, ErrCodexLegacyQuarantine) {
		t.Fatalf("pre-horizon completion = (%d, %T %v), want (0, ErrCodexLegacyQuarantine)", generation, err, err)
	}
	assertCutoverTestFileEqual(t, fsys, "/state/leases.json", quarantinedBytes)

	now = horizon
	for _, test := range []struct {
		name   string
		epochs []uint64
	}{
		{name: "missing", epochs: nil},
		{name: "authoritative_missing", epochs: []uint64{4}},
		{name: "shadow_cannot_reconcile", epochs: []uint64{5}},
		{name: "unsorted", epochs: []uint64{6, 4}},
		{name: "duplicate", epochs: []uint64{4, 4, 6}},
	} {
		t.Run(test.name, func(t *testing.T) {
			generation, completeErr := store.CompleteLegacyCutover(8, CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: test.epochs})
			if generation != 0 || !errors.Is(completeErr, ErrCodexLegacyQuarantine) {
				t.Fatalf("completion = (%d, %T %v), want (0, ErrCodexLegacyQuarantine)", generation, completeErr, completeErr)
			}
			assertCutoverTestFileEqual(t, fsys, "/state/leases.json", quarantinedBytes)
		})
	}

	generation, err := store.CompleteLegacyCutover(8, CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{4, 6}})
	if err != nil {
		t.Fatal(err)
	}
	if generation != 9 || store.Generation() != 9 {
		t.Fatalf("completion generation = returned %d stored %d, want 9", generation, store.Generation())
	}
	completedBytes := readCutoverTestFile(t, fsys, "/state/leases.json")
	if bytes.Equal(completedBytes, quarantinedBytes) {
		t.Fatal("cutover completion did not replace the journal")
	}
	var completed codexLeaseJournalEnvelopeV2
	if err := decodeCodexLeaseStrictJSON(completedBytes, &completed); err != nil {
		t.Fatal(err)
	}
	if completed.Generation != 9 || completed.Cutover.JournalGeneration != 8 || completed.Cutover.State != CodexLeaseCutoverComplete || completed.Cutover.CompletedAt != horizon || completed.Cutover.CompletionGeneration != 9 || !completed.Cutover.NoLegacyAuthority {
		t.Fatalf("completed cutover = generation %d, tuple %#v", completed.Generation, completed.Cutover)
	}
	if completed.Cutover.LegacyQuarantineUntil != horizon || completed.Cutover.LegacyV1SHA256 != digest || len(completed.Lanes) != 0 || len(completed.Records) != 0 || !validCodexLeaseV2TestMAC(key, completed) {
		t.Fatalf("completed evidence changed: cutover=%#v lanes=%d records=%d mac-valid=%t", completed.Cutover, len(completed.Lanes), len(completed.Records), validCodexLeaseV2TestMAC(key, completed))
	}

	evidence, err := store.AuthorityEvidence()
	if err != nil {
		t.Fatal(err)
	}
	if evidence.LeaseSchemaVersion != 3 || evidence.HashVersion != 1 || evidence.CompatibilityEpoch != 4 || evidence.SourceVersion != 1 || evidence.JournalGeneration != 9 || evidence.Health != CodexLeaseAuthorityHealthy || evidence.CutoverState != CodexLeaseCutoverStateComplete || evidence.CutoverAt != time.Date(2026, 8, 9, 4, 5, 6, 700, time.UTC) || evidence.CutoverCompletedAt != horizon || evidence.CutoverCompletionGeneration != 9 || !evidence.NoLegacyAuthority || evidence.LegacyPinnedHorizon != horizon || evidence.LegacyV1ArchiveDigest != digest {
		t.Fatalf("authority evidence = %#v", evidence)
	}
	if !slices.Equal(evidence.AuthoritativeModeEpochs, []uint64{4, 6}) || !slices.Equal(evidence.ShadowModeEpochs, []uint64{5}) || len(evidence.RepresentedAuthoritativeModeEpochs) != 0 {
		t.Fatalf("authority evidence epochs = authoritative %v shadow %v represented %v", evidence.AuthoritativeModeEpochs, evidence.ShadowModeEpochs, evidence.RepresentedAuthoritativeModeEpochs)
	}

	beforeIdempotent := append([]byte(nil), completedBytes...)
	if generation, err := store.CompleteLegacyCutover(9, CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{4, 6}}); err != nil || generation != 9 {
		t.Fatalf("idempotent completion = (%d, %v), want (9, nil)", generation, err)
	}
	assertCutoverTestFileEqual(t, fsys, "/state/leases.json", beforeIdempotent)
	if generation, err := store.CompleteLegacyCutover(8, CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{4, 6}}); generation != 0 || !errors.Is(err, ErrCodexLeaseStaleMutation) {
		t.Fatalf("stale completion = (%d, %T %v), want (0, ErrCodexLeaseStaleMutation)", generation, err, err)
	}
	assertCutoverTestFileEqual(t, fsys, "/state/leases.json", beforeIdempotent)
}

func TestCodexLeaseV2CompleteLegacyCutoverDetachesModeAuthorityBeforeOwnerCallbacks(t *testing.T) {
	fsys := fsutil.NewMemFS()
	now := time.Date(2026, 8, 9, 4, 5, 6, 700, time.UTC)
	writeCodexLeaseV1Fixture(t, fsys)
	owner := &cutoverTestOwner{}
	coordinator, err := OpenCodexContinuityCoordinator(CodexContinuityOpenOptions{
		FS:          fsys,
		KeyPath:     "/state/leases.key",
		JournalPath: "/state/leases.json",
		Policy: CodexLeasePolicy{
			Retention: time.Nanosecond,
			Now:       func() time.Time { return now },
		},
		Modes: CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{4, 6}},
	}, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	now = now.Add(time.Nanosecond)

	modes := []uint64{4, 6}
	owner.setMutation(func() { modes[0] = 5 })
	if generation, err := coordinator.Store().CompleteLegacyCutover(8, CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: modes}); err != nil || generation != 9 {
		t.Fatalf("completion with callback-mutated caller modes = (%d, %T %v), want (9, nil)", generation, err, err)
	}
	if !slices.Equal(coordinator.Store().modes.RecognisedAuthoritativeEpochs, []uint64{4, 6}) {
		t.Fatalf("completed store modes = %v, want detached [4 6]", coordinator.Store().modes.RecognisedAuthoritativeEpochs)
	}

	modes = []uint64{4, 6}
	owner.setMutation(func() { modes[1] = 7 })
	if generation, err := coordinator.Store().CompleteLegacyCutover(9, CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: modes}); err != nil || generation != 9 {
		t.Fatalf("idempotent completion with callback-mutated caller modes = (%d, %T %v), want (9, nil)", generation, err, err)
	}
	if !slices.Equal(coordinator.Store().modes.RecognisedAuthoritativeEpochs, []uint64{4, 6}) {
		t.Fatalf("idempotent store modes = %v, want detached [4 6]", coordinator.Store().modes.RecognisedAuthoritativeEpochs)
	}
}

func TestCodexLeaseV2IdempotentCutoverCannotDropRepresentedModeAuthority(t *testing.T) {
	fsys := fsutil.NewMemFS()
	now := time.Date(2026, 8, 9, 4, 5, 6, 700, time.UTC)
	writeCodexLeaseV1Fixture(t, fsys)
	coordinator, err := OpenCodexContinuityCoordinator(CodexContinuityOpenOptions{
		FS:          fsys,
		KeyPath:     "/state/leases.key",
		JournalPath: "/state/leases.json",
		Policy: CodexLeasePolicy{
			Retention: time.Nanosecond,
			Now:       func() time.Time { return now },
		},
		Modes: CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{4, 6, 9}},
	}, &cutoverTestOwner{})
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	store := coordinator.Store()
	now = now.Add(time.Nanosecond)
	if generation, err := store.CompleteLegacyCutover(8, CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{4, 6, 9}}); err != nil || generation != 9 {
		t.Fatalf("initial completion = (%d, %T %v)", generation, err, err)
	}
	record := reservingCodexLeaseV2CASTestRecord(store, "represented-session", "represented-thread", "represented-turn")
	if _, err := store.CommitLane(CodexLeaseGenerationFence{
		Journal:        store.Generation(),
		TouchedRecords: []CodexLeaseRecordFence{{Record: record.Identity()}},
	}, CodexLaneMutation{Lane: codexLeaseV2CASTestLane(record), UpsertRecords: []CodexJournalRecordV2{record}}); err != nil {
		t.Fatal(err)
	}
	before := append([]byte(nil), store.journalBytes...)
	beforeGeneration := store.Generation()
	beforeModes := cloneCodexModeSnapshot(store.modes)

	if generation, err := store.CompleteLegacyCutover(beforeGeneration, CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{4, 6}}); generation != 0 || !errors.Is(err, ErrCodexLeaseAuthorityMismatch) {
		t.Fatalf("idempotent authority shrink = (%d, %T %v), want authority mismatch", generation, err, err)
	}
	if store.Generation() != beforeGeneration || !bytes.Equal(store.journalBytes, before) || !reflect.DeepEqual(store.modes, beforeModes) || store.poisoned != nil {
		t.Fatalf("rejected mode shrink changed authority: generation=%d modes=%v poison=%v", store.Generation(), store.modes.RecognisedAuthoritativeEpochs, store.poisoned)
	}
}

func TestCodexLeaseV2CutoverPreservesOverlappingLegacyModeEpochs(t *testing.T) {
	fsys := fsutil.NewMemFS()
	now := time.Date(2026, 8, 9, 4, 5, 6, 700, time.UTC)
	retention := 24 * time.Hour
	key, v1Bytes := writeCodexLeaseV1Fixture(t, fsys)
	overlapBytes := rewriteCutoverTestV1(t, key, v1Bytes, true, func(envelope *codexLeaseJournalEnvelope) {
		for _, record := range envelope.Records {
			if !record.Authoritative {
				record.ModeEpoch = 4
				envelope.Records = append(envelope.Records, record)
				return
			}
		}
		t.Fatal("v1 fixture has no shadow record")
	})
	if err := fsys.WriteFile("/state/leases.json", overlapBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := codexLeaseSHA256(overlapBytes)
	coordinator, err := OpenCodexContinuityCoordinator(CodexContinuityOpenOptions{
		FS:          fsys,
		KeyPath:     "/state/leases.key",
		JournalPath: "/state/leases.json",
		Policy: CodexLeasePolicy{
			Retention: retention,
			Now:       func() time.Time { return now },
		},
		Modes: CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{4, 6}},
	}, &cutoverTestOwner{})
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()

	archivePath := filepath.Join("/state", "leases.json.v1-"+digest+".archive")
	assertCutoverTestFileEqual(t, fsys, archivePath, overlapBytes)
	quarantine, err := coordinator.Store().AuthorityEvidence()
	if err != nil {
		t.Fatal(err)
	}
	if quarantine.CutoverState != CodexLeaseCutoverStateLegacyQuarantine || quarantine.LegacyV1ArchiveDigest != digest || !slices.Equal(quarantine.AuthoritativeModeEpochs, []uint64{4, 6}) || !slices.Equal(quarantine.ShadowModeEpochs, []uint64{4, 5}) {
		t.Fatalf("quarantine evidence = %#v", quarantine)
	}

	now = now.Add(retention)
	if generation, err := coordinator.Store().CompleteLegacyCutover(8, CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{4, 6}}); err != nil || generation != 9 {
		t.Fatalf("overlap completion = (%d, %T %v), want (9, nil)", generation, err, err)
	}
	complete, err := coordinator.Store().AuthorityEvidence()
	if err != nil {
		t.Fatal(err)
	}
	if complete.CutoverState != CodexLeaseCutoverStateComplete || !complete.NoLegacyAuthority || complete.CutoverCompletionGeneration != 9 || !slices.Equal(complete.AuthoritativeModeEpochs, []uint64{4, 6}) || !slices.Equal(complete.ShadowModeEpochs, []uint64{4, 5}) {
		t.Fatalf("complete evidence = %#v", complete)
	}
	assertCutoverTestFileEqual(t, fsys, archivePath, overlapBytes)
}

func TestCodexLeaseV2CutoverValidatesExactLegacyAuthority(t *testing.T) {
	fsys := fsutil.NewMemFS()
	key, v1Bytes := writeCodexLeaseV1Fixture(t, fsys)
	store := &CodexLeaseStore{key: key}
	cutover := CodexLeaseCutover{
		SourceVersion:           1,
		JournalGeneration:       8,
		AuthoritativeModeEpochs: []uint64{4, 6},
		ShadowModeEpochs:        []uint64{5},
		LegacyV1SHA256:          codexLeaseSHA256(v1Bytes),
	}
	if err := validateCodexLeaseLegacyCutoverArchive(store, cutover, v1Bytes); err != nil {
		t.Fatalf("valid archive rejected: %v", err)
	}

	tests := []struct {
		name    string
		archive func(*testing.T) []byte
		cutover func(CodexLeaseCutover) CodexLeaseCutover
	}{
		{
			name: "bad_mac",
			archive: func(t *testing.T) []byte {
				return rewriteCutoverTestV1(t, key, v1Bytes, false, func(envelope *codexLeaseJournalEnvelope) { envelope.MAC = "invalid" })
			},
		},
		{
			name: "wrong_generation_with_valid_mac",
			archive: func(t *testing.T) []byte {
				return rewriteCutoverTestV1(t, key, v1Bytes, true, func(envelope *codexLeaseJournalEnvelope) { envelope.Generation = 6 })
			},
		},
		{
			name:    "authoritative_epoch_set",
			archive: func(t *testing.T) []byte { return v1Bytes },
			cutover: func(cutover CodexLeaseCutover) CodexLeaseCutover {
				cutover.AuthoritativeModeEpochs = []uint64{4}
				return cutover
			},
		},
		{
			name:    "shadow_epoch_set",
			archive: func(t *testing.T) []byte { return v1Bytes },
			cutover: func(cutover CodexLeaseCutover) CodexLeaseCutover {
				cutover.ShadowModeEpochs = nil
				return cutover
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			archive := test.archive(t)
			candidateCutover := cutover
			candidateCutover.LegacyV1SHA256 = codexLeaseSHA256(archive)
			if test.cutover != nil {
				candidateCutover = test.cutover(candidateCutover)
			}
			if err := validateCodexLeaseLegacyCutoverArchive(store, candidateCutover, archive); !errors.Is(err, ErrCodexLeaseTrustLost) {
				t.Fatalf("validation error = %T %v, want ErrCodexLeaseTrustLost", err, err)
			}
		})
	}
}

func TestCodexLeaseV2AuthorityEvidenceReturnsZeroAfterPoison(t *testing.T) {
	fsys := fsutil.NewMemFS()
	now := time.Date(2026, 8, 9, 4, 5, 6, 700, time.UTC)
	_, v1Bytes := writeCodexLeaseV1Fixture(t, fsys)
	coordinator, err := OpenCodexContinuityCoordinator(CodexContinuityOpenOptions{
		FS:          fsys,
		KeyPath:     "/state/leases.key",
		JournalPath: "/state/leases.json",
		Policy: CodexLeasePolicy{
			Retention: 24 * time.Hour,
			Now:       func() time.Time { return now },
		},
		Modes: CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{4, 6}},
	}, &cutoverTestOwner{})
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()

	evidence, err := coordinator.Store().AuthorityEvidence()
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Health != CodexLeaseAuthorityHealthy || evidence.CutoverState != CodexLeaseCutoverStateLegacyQuarantine || evidence.JournalGeneration != 8 || evidence.NoLegacyAuthority || evidence.CutoverCompletionGeneration != 0 || !evidence.CutoverCompletedAt.IsZero() || !slices.Equal(evidence.AuthoritativeModeEpochs, []uint64{4, 6}) || !slices.Equal(evidence.ShadowModeEpochs, []uint64{5}) {
		t.Fatalf("quarantine evidence = %#v", evidence)
	}

	archivePath := filepath.Join("/state", "leases.json.v1-"+codexLeaseSHA256(v1Bytes)+".archive")
	if err := fsys.WriteFile(archivePath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	zero, err := coordinator.Store().AuthorityEvidence()
	if err == nil || !reflect.DeepEqual(zero, CodexLeaseAuthorityEvidence{}) {
		t.Fatalf("poisoning evidence = (%#v, %v), want zero plus error", zero, err)
	}
	zero, err = coordinator.Store().AuthorityEvidence()
	if !errors.Is(err, ErrCodexLeaseStorePoisoned) || !reflect.DeepEqual(zero, CodexLeaseAuthorityEvidence{}) {
		t.Fatalf("poisoned evidence = (%#v, %T %v), want zero plus ErrCodexLeaseStorePoisoned", zero, err, err)
	}
}

func TestCodexLeaseV2CutoverAndEvidenceRequireGuardedOwnerOperation(t *testing.T) {
	fsys := fsutil.NewMemFS()
	now := time.Date(2026, 8, 9, 4, 5, 6, 700, time.UTC)
	_, _ = writeCodexLeaseV1Fixture(t, fsys)
	owner := &cutoverTestOwner{}
	coordinator, err := OpenCodexContinuityCoordinator(CodexContinuityOpenOptions{
		FS:          fsys,
		KeyPath:     "/state/leases.key",
		JournalPath: "/state/leases.json",
		Policy: CodexLeasePolicy{
			Retention: time.Nanosecond,
			Now:       func() time.Time { return now },
		},
		Modes: CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{4, 6}},
	}, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	before := readCutoverTestFile(t, fsys, "/state/leases.json")
	now = now.Add(time.Nanosecond)

	beginErr := errors.New("owner operation revoked")
	owner.setBeginError(beginErr)
	if generation, err := coordinator.Store().CompleteLegacyCutover(8, CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{4, 6}}); generation != 0 || !errors.Is(err, ErrCodexLeaseWriterUnavailable) || !errors.Is(err, beginErr) {
		t.Fatalf("completion = (%d, %T %v), want guarded owner error", generation, err, err)
	}
	if evidence, err := coordinator.Store().AuthorityEvidence(); !reflect.DeepEqual(evidence, CodexLeaseAuthorityEvidence{}) || !errors.Is(err, ErrCodexLeaseWriterUnavailable) || !errors.Is(err, beginErr) {
		t.Fatalf("evidence = (%#v, %T %v), want zero plus guarded owner error", evidence, err, err)
	}
	assertCutoverTestFileEqual(t, fsys, "/state/leases.json", before)
	if owner.beginCount() < 3 {
		t.Fatalf("BeginOwnerOperation calls = %d, want open plus both methods", owner.beginCount())
	}
}

type cutoverTestOwner struct {
	mu       sync.Mutex
	beginErr error
	begins   int
	mutate   func()
}

func (owner *cutoverTestOwner) AssertOwner() error { return nil }

func (owner *cutoverTestOwner) BeginOwnerOperation() (*codex.CredentialOwnerOperation, error) {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	owner.begins++
	if owner.mutate != nil {
		owner.mutate()
	}
	if owner.beginErr != nil {
		return nil, owner.beginErr
	}
	return &codex.CredentialOwnerOperation{}, nil
}

func (owner *cutoverTestOwner) setMutation(mutate func()) {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	owner.mutate = mutate
}

func (owner *cutoverTestOwner) setBeginError(err error) {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	owner.beginErr = err
}

func (owner *cutoverTestOwner) beginCount() int {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	return owner.begins
}

func rewriteCutoverTestV1(t *testing.T, key, data []byte, resign bool, mutate func(*codexLeaseJournalEnvelope)) []byte {
	t.Helper()
	var envelope codexLeaseJournalEnvelope
	if err := decodeCodexLeaseStrictJSON(data, &envelope); err != nil {
		t.Fatal(err)
	}
	mutate(&envelope)
	if resign {
		envelope.MAC = ""
		payload, err := json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write(payload)
		envelope.MAC = base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	}
	result, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func readCutoverTestFile(t *testing.T, fsys *fsutil.MemFS, path string) []byte {
	t.Helper()
	data, err := fsys.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertCutoverTestFileEqual(t *testing.T, fsys *fsutil.MemFS, path string, want []byte) {
	t.Helper()
	got := readCutoverTestFile(t, fsys, path)
	if !bytes.Equal(got, want) {
		t.Fatalf("%s changed unexpectedly\nwant: %s\n got: %s", path, want, got)
	}
}
