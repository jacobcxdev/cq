package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/compat"
	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestCodexLeaseV3NewJournalRejectsOldReaderAfterFloorAdvance(t *testing.T) {
	t.Parallel()
	if compat.CurrentEpoch != 4 {
		t.Fatalf("compatibility epoch = %d, want 4", compat.CurrentEpoch)
	}
	fsys := fsutil.NewMemFS()
	options := testCodexContinuityOptions(fsys)
	if err := InitialiseCodexContinuityAuthority(options, testCodexLeaseOwner{}); err != nil {
		t.Fatal(err)
	}
	journal, err := fsys.ReadFile(options.JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	version, err := decodeCodexLeaseVersion(journal)
	if err != nil {
		t.Fatal(err)
	}
	if version != 3 {
		t.Fatalf("new journal version = %d, want 3", version)
	}
	if err := rejectCodexLeaseV3FromV2Reader(journal); err == nil {
		t.Fatal("schema-v2 reader accepted schema-v3 journal")
	}
}

func TestCodexLeaseV3MigratesCanonicalV2WithoutAuthorityLoss(t *testing.T) {
	t.Parallel()
	fsys := fsutil.NewMemFS()
	old, oldBytes := installCanonicalCodexLeaseV2Fixture(t, fsys)
	options := testCodexContinuityOptions(fsys)
	options.Modes = CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{1}}
	coordinator, err := OpenCodexContinuityCoordinator(options, testCodexLeaseOwner{})
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()

	got := cloneCodexLeaseV2Envelope(*coordinator.Store().v2)
	want := cloneCodexLeaseV2Envelope(old)
	want.Version = 3
	want.Cutover.CompatibilityEpoch = 4
	for index := range want.Records {
		want.Records[index].ProtocolSchema = 3
	}
	got.MAC = ""
	want.MAC = ""
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("migrated authority differs:\n got: %#v\nwant: %#v", got, want)
	}
	archivePath := filepath.Join("/state", "leases.json.v2-"+codexLeaseSHA256(oldBytes)+".archive")
	archive, err := fsys.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(archive, oldBytes) {
		t.Fatal("schema-v2 migration archive is not byte-identical")
	}
}

func TestCodexLeaseV3MigrationNormalisesCrashedActiveRecordOnFirstOpen(t *testing.T) {
	t.Parallel()
	fsys := fsutil.NewMemFS()
	store, envelope := codexLeaseV2SchemaFixture(t)
	envelope.Version = 2
	envelope.Cutover.CompatibilityEpoch = 3
	record := &envelope.Records[0]
	record.ProtocolSchema = 2
	record.State = LeaseBoundActive
	record.Attempts[0].State = CodexAttemptStreaming
	codexLeaseV2SetSchemaAdmissionEvidence(&envelope, envelope.Generation)
	envelope.Lanes[0].LastCacheAdmittedAt = time.Time{}
	envelope.Lanes[0].LastCacheEffectiveModel = ""
	codexLeaseV2SignSchemaFixture(t, store, &envelope)
	journal := codexLeaseV2SchemaJSON(t, envelope)
	if err := fsys.MkdirAll("/state", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("/state/leases.key", store.key, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("/state/leases.json", journal, 0o600); err != nil {
		t.Fatal(err)
	}
	options := testCodexContinuityOptions(fsys)
	options.Modes = CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{1}}
	coordinator, err := OpenCodexContinuityCoordinator(options, testCodexLeaseOwner{})
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()

	got := coordinator.Store().v2
	if got.Version != 3 || got.Generation != envelope.Generation+1 {
		t.Fatalf("normalised migration version/generation = %d/%d, want 3/%d", got.Version, got.Generation, envelope.Generation+1)
	}
	restored := got.Records[0]
	if restored.State != LeaseOrphaned || !restored.SocketLineageExtinct || restored.RoutingRefs != 0 || restored.AttemptRefs != 0 || restored.ResponseObserverRefs != 0 || !restored.NonMigratable {
		t.Fatalf("normalised crashed record = %#v", restored)
	}
	if restored.RecordGeneration != record.RecordGeneration+1 || restored.LeaseGeneration != record.LeaseGeneration+1 || restored.CurrentAttemptGeneration != record.CurrentAttemptGeneration {
		t.Fatalf("normalised record generations = record %d lease %d attempt %d", restored.RecordGeneration, restored.LeaseGeneration, restored.CurrentAttemptGeneration)
	}
	attempt := restored.Attempts[0]
	if attempt.State != CodexAttemptIndeterminate || attempt.Revision != record.Attempts[0].Revision+1 {
		t.Fatalf("normalised crashed attempt = %#v", attempt)
	}
	archivePath := filepath.Join("/state", "leases.json.v2-"+codexLeaseSHA256(journal)+".archive")
	archive, archiveErr := fsys.ReadFile(archivePath)
	if archiveErr != nil || !bytes.Equal(archive, journal) {
		t.Fatalf("normalised migration archive = %d bytes/%v, want exact v2", len(archive), archiveErr)
	}
}

func TestCodexLeaseV3MigratesArchiveBackedV2QuarantineLosslessly(t *testing.T) {
	t.Parallel()
	fsys := fsutil.NewMemFS()
	old, oldBytes, legacy := installArchiveBackedCodexLeaseV2Fixture(t, fsys)
	options := testCodexContinuityOptions(fsys)
	options.Policy.Now = func() time.Time { return old.Cutover.LegacyQuarantineUntil }
	coordinator, err := OpenCodexContinuityCoordinator(options, testCodexLeaseOwner{})
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	got := cloneCodexLeaseV2Envelope(*coordinator.Store().v2)
	want := cloneCodexLeaseV2Envelope(old)
	want.Version = 3
	want.Cutover.CompatibilityEpoch = 4
	got.MAC = ""
	want.MAC = ""
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("migrated quarantine differs:\n got: %#v\nwant: %#v", got, want)
	}
	oldArchive, err := fsys.ReadFile(filepath.Join("/state", "leases.json.v2-"+codexLeaseSHA256(oldBytes)+".archive"))
	if err != nil || !bytes.Equal(oldArchive, oldBytes) {
		t.Fatalf("schema-v2 archive = %d bytes/%v, want exact source", len(oldArchive), err)
	}
	legacyArchive, err := fsys.ReadFile(filepath.Join("/state", "leases.json.v1-"+codexLeaseSHA256(legacy)+".archive"))
	if err != nil || !bytes.Equal(legacyArchive, legacy) {
		t.Fatalf("legacy archive = %d bytes/%v, want exact retained source", len(legacyArchive), err)
	}
	completed, err := coordinator.Store().CompleteLegacyCutover(old.Generation, CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{4, 6}})
	if err != nil {
		t.Fatal(err)
	}
	if completed != old.Generation+1 || coordinator.Store().v2.Cutover.CompletionGeneration != old.Generation+1 {
		t.Fatalf("post-migration completion = journal %d evidence %d, want %d", completed, coordinator.Store().v2.Cutover.CompletionGeneration, old.Generation+1)
	}
}

func TestCodexLeaseV3RejectsNonCurrentV2MigrationEpochWithoutMutation(t *testing.T) {
	t.Parallel()
	for _, epoch := range []int{2, 4, 5} {
		t.Run(time.Duration(epoch).String(), func(t *testing.T) {
			fsys := fsutil.NewMemFS()
			envelope, _ := installCanonicalCodexLeaseV2Fixture(t, fsys)
			envelope.Cutover.CompatibilityEpoch = epoch
			store := &CodexLeaseStore{key: bytes.Repeat([]byte{0x5a}, codexLeaseHMACKeyBytes)}
			codexLeaseV2SignSchemaFixture(t, store, &envelope)
			before := codexLeaseV2SchemaJSON(t, envelope)
			if err := fsys.WriteFile("/state/leases.json", before, 0o600); err != nil {
				t.Fatal(err)
			}
			options := testCodexContinuityOptions(fsys)
			options.Modes = CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{1}}
			if coordinator, err := OpenCodexContinuityCoordinator(options, testCodexLeaseOwner{}); coordinator != nil || !errors.Is(err, ErrCodexLeaseTrustLost) {
				t.Fatalf("epoch %d open = %#v/%v, want nil/trust lost", epoch, coordinator, err)
			}
			after, err := fsys.ReadFile(options.JournalPath)
			if err != nil || !bytes.Equal(after, before) {
				t.Fatalf("epoch %d rejection changed journal: %v", epoch, err)
			}
		})
	}
}

func TestCodexLeaseV3MigrationWriteFailuresReturnNoAuthorityAndRemainRetryable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		phase           string
		wantOutcome     fsutil.CommitOutcome
		wantCanonicalV2 bool
	}{
		{name: "pre-rename", phase: "rename", wantOutcome: fsutil.CommitNotCommitted, wantCanonicalV2: true},
		{name: "post-rename indeterminate", phase: "dir-sync", wantOutcome: fsutil.CommitIndeterminate},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mem := fsutil.NewMemFS()
			_, oldBytes := installCanonicalCodexLeaseV2Fixture(t, mem)
			archivePath := filepath.Join("/state", "leases.json.v2-"+codexLeaseSHA256(oldBytes)+".archive")
			fsys := newCodexLeaseMigrationCrashFS(mem, codexLeaseMigrationV2Target, test.phase)
			options := testCodexContinuityOptions(fsys)
			options.Modes = CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{1}}

			coordinator, err := OpenCodexContinuityCoordinator(options, testCodexLeaseOwner{})
			if coordinator != nil || err == nil {
				if coordinator != nil {
					_ = coordinator.Close()
				}
				t.Fatalf("injected migration = %#v/%v, want nil/error", coordinator, err)
			}
			if got := fsutil.AtomicWriteOutcome(err); got != test.wantOutcome {
				t.Fatalf("migration outcome = %s, want %s: %v", got, test.wantOutcome, err)
			}
			archive, archiveErr := mem.ReadFile(archivePath)
			if archiveErr != nil || !bytes.Equal(archive, oldBytes) {
				t.Fatalf("migration recovery archive = %d bytes/%v, want byte-identical v2", len(archive), archiveErr)
			}
			canonical, readErr := mem.ReadFile(options.JournalPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			version, versionErr := decodeCodexLeaseVersion(canonical)
			if versionErr != nil {
				t.Fatal(versionErr)
			}
			if test.wantCanonicalV2 {
				if version != 2 || !bytes.Equal(canonical, oldBytes) {
					t.Fatalf("pre-rename canonical version/bytes = %d/%t, want exact v2", version, bytes.Equal(canonical, oldBytes))
				}
			} else if version != 3 {
				t.Fatalf("post-rename canonical version = %d, want 3", version)
			}

			fsys.disableFailure()
			reopened, reopenErr := OpenCodexContinuityCoordinator(options, testCodexLeaseOwner{})
			if reopenErr != nil {
				t.Fatalf("retry after %s: %v", test.phase, reopenErr)
			}
			if reopened.Store().v2 == nil || reopened.Store().v2.Version != 3 {
				t.Fatalf("retry authority = %#v, want v3", reopened.Store().v2)
			}
			if closeErr := reopened.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
		})
	}
}

func TestCodexLeaseV3RejectsNoncanonicalV2MigrationSourceWithoutMutation(t *testing.T) {
	t.Parallel()
	fsys := fsutil.NewMemFS()
	envelope, _ := installCanonicalCodexLeaseV2Fixture(t, fsys)
	envelope.Lanes[0].LastCacheAdmittedAt = time.Date(2026, 8, 9, 1, 2, 3, 0, time.UTC)
	envelope.Lanes[0].LastCacheEffectiveModel = "gpt-5.6-sol"
	store := &CodexLeaseStore{key: bytes.Repeat([]byte{0x5a}, codexLeaseHMACKeyBytes)}
	codexLeaseV2SignSchemaFixture(t, store, &envelope)
	before := codexLeaseV2SchemaJSON(t, envelope)
	if err := fsys.WriteFile("/state/leases.json", before, 0o600); err != nil {
		t.Fatal(err)
	}
	options := testCodexContinuityOptions(fsys)
	options.Modes = CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{1}}
	if coordinator, err := OpenCodexContinuityCoordinator(options, testCodexLeaseOwner{}); coordinator != nil || !errors.Is(err, ErrCodexLeaseTrustLost) {
		t.Fatalf("noncanonical v2 open = %#v/%v, want nil/trust lost", coordinator, err)
	}
	after, err := fsys.ReadFile(options.JournalPath)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("noncanonical v2 rejection changed journal: %v", err)
	}
	archivePath := filepath.Join("/state", "leases.json.v2-"+codexLeaseSHA256(before)+".archive")
	if _, err := fsys.ReadFile(archivePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("noncanonical v2 rejection wrote archive: %v", err)
	}
}

func TestCodexLeaseV3RejectsExplicitZeroCacheKeysInV2Source(t *testing.T) {
	t.Parallel()
	fsys := fsutil.NewMemFS()
	_, canonical := installCanonicalCodexLeaseV2Fixture(t, fsys)
	mutated := bytes.Replace(
		canonical,
		[]byte(`"lanes":[{`),
		[]byte(`"lanes":[{"last_cache_admitted_at":"0001-01-01T00:00:00Z","last_cache_effective_model":"",`),
		1,
	)
	if bytes.Equal(mutated, canonical) {
		t.Fatal("explicit zero cache-key mutation did not change the v2 fixture")
	}
	var decoded codexLeaseJournalEnvelopeV2
	if err := json.Unmarshal(mutated, &decoded); err != nil {
		t.Fatal(err)
	}
	store := &CodexLeaseStore{key: bytes.Repeat([]byte{0x5a}, codexLeaseHMACKeyBytes)}
	if !store.validV2EnvelopeMAC(decoded) {
		t.Fatal("explicit zero cache keys unexpectedly changed the old canonical MAC")
	}
	if err := fsys.WriteFile("/state/leases.json", mutated, 0o600); err != nil {
		t.Fatal(err)
	}
	options := testCodexContinuityOptions(fsys)
	options.Modes = CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{1}}
	if coordinator, err := OpenCodexContinuityCoordinator(options, testCodexLeaseOwner{}); coordinator != nil || !errors.Is(err, ErrCodexLeaseTrustLost) {
		t.Fatalf("explicit zero cache keys open = %#v/%v, want nil/trust lost", coordinator, err)
	}
	after, err := fsys.ReadFile(options.JournalPath)
	if err != nil || !bytes.Equal(after, mutated) {
		t.Fatalf("explicit zero cache-key rejection changed journal: %v", err)
	}
	archivePath := filepath.Join("/state", "leases.json.v2-"+codexLeaseSHA256(mutated)+".archive")
	if _, err := fsys.ReadFile(archivePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("explicit zero cache-key rejection wrote archive: %v", err)
	}
}

func rejectCodexLeaseV3FromV2Reader(journal []byte) error {
	version, err := decodeCodexLeaseVersion(journal)
	if err != nil {
		return err
	}
	if version != 2 {
		return errors.New("unsupported Codex lease journal version")
	}
	return nil
}

func installCanonicalCodexLeaseV2Fixture(t *testing.T, fsys *fsutil.MemFS) (codexLeaseJournalEnvelopeV2, []byte) {
	t.Helper()
	store, envelope := codexLeaseV2SchemaFixture(t)
	record := &envelope.Records[0]
	record.ProtocolSchema = 2
	record.State = LeaseFailedUnadmitted
	record.SocketLineageExtinct = true
	record.RoutingRefs = 0
	record.AttemptRefs = 0
	record.Attempts[0].State = CodexAttemptProviderFailed
	record.Attempts[0].Revision++
	lane := &envelope.Lanes[0]
	lane.CurrentTurnHash = ""
	lane.CurrentModeEpoch = 0
	lane.CurrentAuthoritative = false
	envelope.Version = 2
	envelope.Cutover.CompatibilityEpoch = 3
	codexLeaseV2SignSchemaFixture(t, store, &envelope)
	journal := codexLeaseV2SchemaJSON(t, envelope)
	if err := fsys.MkdirAll("/state", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("/state/leases.key", store.key, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("/state/leases.json", journal, 0o600); err != nil {
		t.Fatal(err)
	}
	return envelope, journal
}

func installArchiveBackedCodexLeaseV2Fixture(t *testing.T, fsys *fsutil.MemFS) (codexLeaseJournalEnvelopeV2, []byte, []byte) {
	t.Helper()
	key, legacy := writeCodexLeaseV1Fixture(t, fsys)
	var legacyEnvelope codexLeaseJournalEnvelope
	if err := decodeCodexLeaseV1StrictJSON(legacy, &legacyEnvelope); err != nil {
		t.Fatal(err)
	}
	authoritative, shadow := codexLeaseV1Epochs(legacyEnvelope.Records)
	now := time.Date(2026, 8, 9, 4, 5, 6, 700, time.UTC)
	envelope := codexLeaseJournalEnvelopeV2{
		Version: 2, HashVersion: codexLeaseHashVersion, Generation: legacyEnvelope.Generation + 1,
		Cutover: CodexLeaseCutover{
			SourceVersion: 1, CompatibilityEpoch: 3, State: CodexLeaseCutoverLegacyQuarantine,
			At: now, JournalGeneration: legacyEnvelope.Generation + 1,
			AuthoritativeModeEpochs: authoritative, ShadowModeEpochs: shadow,
			LegacyQuarantineUntil: now.Add(DefaultCodexLeaseRetention), LegacyV1SHA256: codexLeaseSHA256(legacy),
		},
		Lanes: []CodexJournalLane{}, Records: []CodexJournalRecordV2{},
	}
	store := &CodexLeaseStore{key: append([]byte(nil), key...)}
	codexLeaseV2SignSchemaFixture(t, store, &envelope)
	journal := codexLeaseV2SchemaJSON(t, envelope)
	if err := fsys.WriteFile("/state/leases.json", journal, 0o600); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join("/state", "leases.json.v1-"+codexLeaseSHA256(legacy)+".archive")
	if err := fsys.WriteFile(archivePath, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	return envelope, journal, legacy
}
