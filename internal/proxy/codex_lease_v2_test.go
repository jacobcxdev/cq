package proxy

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexLeaseV2BothMissingRequiresAuthorityWithoutMutation(t *testing.T) {
	t.Parallel()
	fsys := fsutil.NewMemFS()
	_, err := OpenCodexContinuityCoordinator(CodexContinuityOpenOptions{
		FS:          fsys,
		KeyPath:     "/state/leases.key",
		JournalPath: "/state/leases.json",
		Policy: CodexLeasePolicy{
			Retention: 24 * time.Hour,
			Now:       func() time.Time { return time.Date(2026, 8, 9, 3, 0, 0, 0, time.UTC) },
		},
		Modes: CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{}},
	}, testCodexLeaseOwner{})
	if !errors.Is(err, ErrCodexLeaseFreshInstallAuthorityRequired) {
		t.Fatalf("open error = %T %v, want ErrCodexLeaseFreshInstallAuthorityRequired", err, err)
	}
	for _, path := range []string{
		"/state",
		"/state/leases.key",
		"/state/leases.json",
		"/state/leases.json.v1.archive",
	} {
		if _, statErr := fsys.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("fresh-install refusal mutated %s: %v", path, statErr)
		}
	}
}

func TestCodexLeaseV2RequiresExactDetachedModeAuthoritySnapshot(t *testing.T) {
	invalid := []struct {
		name   string
		epochs []uint64
	}{
		{name: "missing", epochs: nil},
		{name: "zero", epochs: []uint64{0}},
		{name: "duplicate", epochs: []uint64{4, 4}},
		{name: "unsorted", epochs: []uint64{6, 4}},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			fsys := fsutil.NewMemFS()
			owner := &countingCodexLeaseTestOwner{}
			options := testCodexContinuityOptions(fsys)
			options.Modes = CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: test.epochs}
			if _, err := OpenCodexContinuityCoordinator(options, owner); !errors.Is(err, ErrCodexLeaseTrustLost) {
				t.Fatalf("mode snapshot open error = %T %v, want trust lost", err, err)
			}
			if owner.asserts != 0 || owner.begins != 0 {
				t.Fatalf("invalid mode snapshot consulted owner: asserts=%d begins=%d", owner.asserts, owner.begins)
			}
			if _, err := fsys.Stat("/state"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid mode snapshot touched filesystem: %v", err)
			}
		})
	}

	t.Run("non-nil empty is valid", func(t *testing.T) {
		fsys := fsutil.NewMemFS()
		owner := &countingCodexLeaseTestOwner{}
		options := testCodexContinuityOptions(fsys)
		options.Modes = CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{}}
		if _, err := OpenCodexContinuityCoordinator(options, owner); !errors.Is(err, ErrCodexLeaseFreshInstallAuthorityRequired) {
			t.Fatalf("empty mode snapshot open error = %T %v, want fresh-install authority", err, err)
		}
		if owner.asserts != 1 || owner.begins != 1 {
			t.Fatalf("valid empty mode snapshot owner calls = asserts %d begins %d", owner.asserts, owner.begins)
		}
	})

	t.Run("sorted snapshot is detached", func(t *testing.T) {
		fsys := fsutil.NewMemFS()
		writeCodexLeaseV1Fixture(t, fsys)
		epochs := []uint64{4, 6}
		options := testCodexContinuityOptions(fsys)
		options.Modes = CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: epochs}
		coordinator, err := OpenCodexContinuityCoordinator(options, testCodexLeaseOwner{})
		if err != nil {
			t.Fatal(err)
		}
		defer coordinator.Close()
		epochs[0] = 99
		if !slices.Equal(coordinator.Store().modes.RecognisedAuthoritativeEpochs, []uint64{4, 6}) {
			t.Fatalf("stored mode snapshot aliased caller: %v", coordinator.Store().modes.RecognisedAuthoritativeEpochs)
		}
	})

	t.Run("owner callback cannot mutate validated snapshot", func(t *testing.T) {
		fsys := fsutil.NewMemFS()
		writeCodexLeaseV1Fixture(t, fsys)
		epochs := []uint64{4, 6}
		options := testCodexContinuityOptions(fsys)
		options.Modes = CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: epochs}
		owner := mutatingCodexLeaseTestOwner{mutate: func() { epochs[0] = 0 }}
		coordinator, err := OpenCodexContinuityCoordinator(options, owner)
		if err != nil {
			t.Fatal(err)
		}
		defer coordinator.Close()
		if !slices.Equal(coordinator.Store().modes.RecognisedAuthoritativeEpochs, []uint64{4, 6}) {
			t.Fatalf("owner callback changed stored mode snapshot: %v", coordinator.Store().modes.RecognisedAuthoritativeEpochs)
		}
	})
}

func TestCodexLeaseV2MigratesV1ToArchiveAndGlobalQuarantine(t *testing.T) {
	t.Parallel()
	fsys := fsutil.NewMemFS()
	now := time.Date(2026, 8, 9, 4, 5, 6, 700, time.UTC)
	retention := 2 * 24 * time.Hour
	key, v1Bytes := writeCodexLeaseV1Fixture(t, fsys)
	digestBytes := sha256.Sum256(v1Bytes)
	digest := hex.EncodeToString(digestBytes[:])

	coordinator, err := OpenCodexContinuityCoordinator(CodexContinuityOpenOptions{
		FS:          fsys,
		KeyPath:     "/state/leases.key",
		JournalPath: "/state/leases.json",
		Policy: CodexLeasePolicy{
			Retention: retention,
			Now:       func() time.Time { return now },
		},
		Modes: CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{4, 6}},
	}, testCodexLeaseOwner{})
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()

	archivePath := filepath.Join("/state", "leases.json.v1-"+digest+".archive")
	archive, err := fsys.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(archive, v1Bytes) {
		t.Fatal("v1 archive is not byte-identical")
	}
	info, err := fsys.Stat(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("archive mode = %04o, want 0600", info.Mode().Perm())
	}

	journal, err := fsys.ReadFile("/state/leases.json")
	if err != nil {
		t.Fatal(err)
	}
	var envelope codexLeaseJournalEnvelopeV2
	if err := json.Unmarshal(journal, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Version != 2 || envelope.HashVersion != 1 || envelope.Generation != 8 {
		t.Fatalf("v2 envelope versions/generation = %d/%d/%d, want 2/1/8", envelope.Version, envelope.HashVersion, envelope.Generation)
	}
	cutover := envelope.Cutover
	if cutover.SourceVersion != 1 || cutover.CompatibilityEpoch != 3 || cutover.State != CodexLeaseCutoverLegacyQuarantine || cutover.At != now || cutover.JournalGeneration != 8 || cutover.LegacyQuarantineUntil != now.Add(retention) || cutover.LegacyV1SHA256 != digest || cutover.NoLegacyAuthority || !cutover.CompletedAt.IsZero() || cutover.CompletionGeneration != 0 {
		t.Fatalf("cutover = %#v", cutover)
	}
	if !slices.Equal(cutover.AuthoritativeModeEpochs, []uint64{4, 6}) || !slices.Equal(cutover.ShadowModeEpochs, []uint64{5}) {
		t.Fatalf("cutover epochs = authoritative %v shadow %v", cutover.AuthoritativeModeEpochs, cutover.ShadowModeEpochs)
	}
	if len(envelope.Lanes) != 0 || len(envelope.Records) != 0 || envelope.MAC == "" {
		t.Fatalf("migrated envelope retained routable state: lanes=%d records=%d mac=%q", len(envelope.Lanes), len(envelope.Records), envelope.MAC)
	}
	for _, raw := range []string{"session-one", "thread-one", "turn-one", "session-two", "thread-two", "turn-shadow", "turn-two", "account-one", "response-one", "state-one"} {
		if strings.Contains(string(journal), raw) {
			t.Fatalf("v2 journal leaked raw fixture value %q", raw)
		}
	}
	if !validCodexLeaseV2TestMAC(key, envelope) {
		t.Fatal("migrated v2 MAC is invalid")
	}
	_, err = coordinator.Store().LoadLane(
		testCodexLeaseKey("thread-one", "turn-one"),
		[]codex.AccountKey{"account-one"},
		CodexLeaseAuthorityPolicy{ModeEpoch: 6, Authoritative: true},
	)
	if !errors.Is(err, ErrCodexLegacyQuarantine) {
		t.Fatalf("LoadLane error = %T %v, want ErrCodexLegacyQuarantine", err, err)
	}
}

func TestCodexLeaseV2RejectsInvalidMigrationCandidateBeforeWriting(t *testing.T) {
	fsys := fsutil.NewMemFS()
	_, original := writeCodexLeaseV1Fixture(t, fsys)
	options := testCodexContinuityOptions(fsys)
	options.Policy.Now = func() time.Time { return time.Time{} }

	if _, err := OpenCodexContinuityCoordinator(options, testCodexLeaseOwner{}); !errors.Is(err, ErrCodexLeaseTrustLost) {
		t.Fatalf("invalid migration candidate error = %T %v, want trust lost", err, err)
	}
	journal, err := fsys.ReadFile("/state/leases.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(journal, original) {
		t.Fatal("invalid migration candidate replaced the legacy journal")
	}
	entries, err := fsys.ReadDir("/state")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".archive") {
			t.Fatalf("invalid migration candidate created archive %q", entry.Name())
		}
	}
}

func TestCodexLeaseV2RejectsUnsafeExistingDirectoryBeforeFreshClassification(t *testing.T) {
	t.Parallel()
	fsys := fsutil.NewMemFS()
	if err := fsys.MkdirAll("/state", 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := OpenCodexContinuityCoordinator(testCodexContinuityOptions(fsys), testCodexLeaseOwner{})
	if !errors.Is(err, ErrCodexLeaseTrustLost) {
		t.Fatalf("open error = %T %v, want ErrCodexLeaseTrustLost", err, err)
	}
	info, statErr := fsys.Stat("/state")
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("directory mode changed to %04o", info.Mode().Perm())
	}
}

func TestCodexLeaseV2IncompletePairFailsClosedWithoutCleanup(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		path string
		data []byte
	}{
		{name: "key only", path: "/state/leases.key", data: bytes.Repeat([]byte{0x31}, codexLeaseHMACKeyBytes)},
		{name: "journal only", path: "/state/leases.json", data: []byte(`{"version":1}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			fsys := fsutil.NewMemFS()
			if err := fsys.MkdirAll("/state", 0o700); err != nil {
				t.Fatal(err)
			}
			if err := fsys.WriteFile(test.path, test.data, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := OpenCodexContinuityCoordinator(testCodexContinuityOptions(fsys), testCodexLeaseOwner{})
			if !errors.Is(err, ErrCodexLeaseTrustLost) {
				t.Fatalf("open error = %T %v, want ErrCodexLeaseTrustLost", err, err)
			}
			got, readErr := fsys.ReadFile(test.path)
			if readErr != nil || !bytes.Equal(got, test.data) {
				t.Fatalf("incomplete pair was changed: data=%q err=%v", got, readErr)
			}
			other := "/state/leases.key"
			if test.path == other {
				other = "/state/leases.json"
			}
			if _, statErr := fsys.Stat(other); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("missing pair member was created: %v", statErr)
			}
		})
	}
}

func TestCodexLeaseV2RevokedOwnerCannotMigrate(t *testing.T) {
	t.Parallel()
	fsys := fsutil.NewMemFS()
	_, original := writeCodexLeaseV1Fixture(t, fsys)
	ownerErr := errors.New("owner revoked")
	_, err := OpenCodexContinuityCoordinator(testCodexContinuityOptions(fsys), testCodexLeaseOwner{err: ownerErr})
	if !errors.Is(err, ErrCodexLeaseWriterUnavailable) || !errors.Is(err, ownerErr) {
		t.Fatalf("open error = %T %v, want writer unavailable and owner error", err, err)
	}
	journal, readErr := fsys.ReadFile("/state/leases.json")
	if readErr != nil || !bytes.Equal(journal, original) {
		t.Fatalf("revoked owner changed journal: err=%v", readErr)
	}
	entries, readDirErr := fsys.ReadDir("/state")
	if readDirErr != nil {
		t.Fatal(readDirErr)
	}
	if len(entries) != 2 {
		t.Fatalf("revoked owner created files: %v", entries)
	}
}

func TestCodexLeaseV2StrictJSONRejectsUnknownAndDuplicateFieldsWithoutWriting(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{
			name: "unknown",
			mutate: func(data []byte) []byte {
				return bytes.Replace(data, []byte("\n}"), []byte(",\n  \"future_field\": true\n}"), 1)
			},
		},
		{
			name: "duplicate",
			mutate: func(data []byte) []byte {
				return bytes.Replace(data, []byte(`"version": 1`), []byte(`"version": 1, "version": 1`), 1)
			},
		},
		{
			name: "case folded duplicate",
			mutate: func(data []byte) []byte {
				return bytes.Replace(data, []byte(`"version": 1`), []byte(`"version": 1, "Version": 1`), 1)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fsys := fsutil.NewMemFS()
			_, original := writeCodexLeaseV1Fixture(t, fsys)
			mutated := test.mutate(original)
			if bytes.Equal(mutated, original) {
				t.Fatal("fixture mutation did not apply")
			}
			if err := fsys.WriteFile("/state/leases.json", mutated, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := OpenCodexContinuityCoordinator(testCodexContinuityOptions(fsys), testCodexLeaseOwner{})
			if !errors.Is(err, ErrCodexLeaseTrustLost) {
				t.Fatalf("open error = %T %v, want ErrCodexLeaseTrustLost", err, err)
			}
			got, readErr := fsys.ReadFile("/state/leases.json")
			if readErr != nil || !bytes.Equal(got, mutated) {
				t.Fatalf("invalid journal was changed: err=%v", readErr)
			}
		})
	}
}

func TestCodexLeaseV2MigrationRejectsSignedV1ShapeAliasesWithoutWriting(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{
			name: "missing required false",
			mutate: func(data []byte) []byte {
				return bytes.Replace(data, []byte("      \"authoritative\": false,\n"), nil, 1)
			},
		},
		{
			name: "explicit omitted false",
			mutate: func(data []byte) []byte {
				return bytes.Replace(data, []byte("      \"last_seen\":"), []byte("      \"non_migratable\": false,\n      \"last_seen\":"), 1)
			},
		},
		{
			name: "reordered envelope members",
			mutate: func(data []byte) []byte {
				return bytes.Replace(data, []byte("  \"version\": 1,\n  \"generation\": 7,"), []byte("  \"generation\": 7,\n  \"version\": 1,"), 1)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fsys := fsutil.NewMemFS()
			_, original := writeCodexLeaseV1Fixture(t, fsys)
			mutated := test.mutate(original)
			if bytes.Equal(mutated, original) {
				t.Fatal("signed v1 fixture mutation did not apply")
			}
			if err := fsys.WriteFile("/state/leases.json", mutated, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenCodexContinuityCoordinator(testCodexContinuityOptions(fsys), testCodexLeaseOwner{}); !errors.Is(err, ErrCodexLeaseTrustLost) {
				t.Fatalf("signed v1 alias open error = %T %v, want trust lost", err, err)
			}
			stored, err := fsys.ReadFile("/state/leases.json")
			if err != nil || !bytes.Equal(stored, mutated) {
				t.Fatalf("signed v1 alias changed journal: err=%v", err)
			}
			entries, err := fsys.ReadDir("/state")
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 2 {
				t.Fatalf("signed v1 alias created migration files: %v", entries)
			}
		})
	}
}

func TestCodexLeaseV2RejectsNonCanonicalMACAliasesWithoutWriting(t *testing.T) {
	t.Run("legacy v1 migration", func(t *testing.T) {
		fsys := fsutil.NewMemFS()
		_, original := writeCodexLeaseV1Fixture(t, fsys)
		var envelope codexLeaseJournalEnvelope
		if err := json.Unmarshal(original, &envelope); err != nil {
			t.Fatal(err)
		}
		envelope.MAC = nonCanonicalCodexLeaseTestMAC(t, envelope.MAC)
		aliased, err := json.MarshalIndent(envelope, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := fsys.WriteFile("/state/leases.json", aliased, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenCodexContinuityCoordinator(testCodexContinuityOptions(fsys), testCodexLeaseOwner{}); !errors.Is(err, ErrCodexLeaseTrustLost) {
			t.Fatalf("noncanonical v1 MAC open error = %T %v, want trust lost", err, err)
		}
		stored, err := fsys.ReadFile("/state/leases.json")
		if err != nil || !bytes.Equal(stored, aliased) {
			t.Fatalf("noncanonical v1 MAC changed journal: err=%v", err)
		}
		entries, err := fsys.ReadDir("/state")
		if err != nil || len(entries) != 2 {
			t.Fatalf("noncanonical v1 MAC created files: entries=%v err=%v", entries, err)
		}
	})

	t.Run("v2 reopen", func(t *testing.T) {
		fsys := fsutil.NewMemFS()
		writeCodexLeaseV1Fixture(t, fsys)
		coordinator, err := OpenCodexContinuityCoordinator(testCodexContinuityOptions(fsys), testCodexLeaseOwner{})
		if err != nil {
			t.Fatal(err)
		}
		if err := coordinator.Close(); err != nil {
			t.Fatal(err)
		}
		original, err := fsys.ReadFile("/state/leases.json")
		if err != nil {
			t.Fatal(err)
		}
		var envelope codexLeaseJournalEnvelopeV2
		if err := json.Unmarshal(original, &envelope); err != nil {
			t.Fatal(err)
		}
		envelope.MAC = nonCanonicalCodexLeaseTestMAC(t, envelope.MAC)
		aliased, err := json.MarshalIndent(envelope, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := fsys.WriteFile("/state/leases.json", aliased, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenCodexContinuityCoordinator(testCodexContinuityOptions(fsys), testCodexLeaseOwner{}); !errors.Is(err, ErrCodexLeaseTrustLost) {
			t.Fatalf("noncanonical v2 MAC open error = %T %v, want trust lost", err, err)
		}
		stored, err := fsys.ReadFile("/state/leases.json")
		if err != nil || !bytes.Equal(stored, aliased) {
			t.Fatalf("noncanonical v2 MAC changed journal: err=%v", err)
		}
	})
}

func nonCanonicalCodexLeaseTestMAC(t *testing.T, canonical string) string {
	t.Helper()
	want, err := base64.RawURLEncoding.DecodeString(canonical)
	if err != nil {
		t.Fatal(err)
	}
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	for _, candidate := range alphabet {
		alias := canonical[:len(canonical)-1] + string(candidate)
		if alias == canonical {
			continue
		}
		decoded, err := base64.RawURLEncoding.DecodeString(alias)
		if err == nil && bytes.Equal(decoded, want) {
			return alias
		}
	}
	alias := canonical[:1] + "\n" + canonical[1:]
	decoded, err := base64.RawURLEncoding.DecodeString(alias)
	if err == nil && bytes.Equal(decoded, want) {
		return alias
	}
	t.Fatal("base64 decoder exposed no noncanonical alias")
	return ""
}

func TestCodexLeaseV2MigrationReopenIsIdempotentAndLegacyWritersCannotDowngrade(t *testing.T) {
	t.Parallel()
	fsys := fsutil.NewMemFS()
	writeCodexLeaseV1Fixture(t, fsys)
	options := testCodexContinuityOptions(fsys)
	first, err := OpenCodexContinuityCoordinator(options, testCodexLeaseOwner{})
	if err != nil {
		t.Fatal(err)
	}
	journal, err := fsys.ReadFile("/state/leases.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Store().CommitCurrentLeases(nil); !errors.Is(err, ErrCodexLeaseWriterUnavailable) {
		t.Fatalf("legacy snapshot error = %T %v, want writer unavailable", err, err)
	}
	if err := first.Store().Compact(options.Policy.Now(), options.Policy.Retention); !errors.Is(err, ErrCodexLegacyQuarantine) {
		t.Fatalf("quarantined compact error = %T %v, want legacy quarantine", err, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := OpenCodexContinuityCoordinator(options, testCodexLeaseOwner{})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	got, err := fsys.ReadFile("/state/leases.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, journal) {
		t.Fatal("idempotent reopen or rejected legacy writer changed v2 bytes")
	}
}

func testCodexContinuityOptions(fsys fsutil.DurableFileSystem) CodexContinuityOpenOptions {
	return CodexContinuityOpenOptions{
		FS:          fsys,
		KeyPath:     "/state/leases.key",
		JournalPath: "/state/leases.json",
		Policy: CodexLeasePolicy{
			Retention: 24 * time.Hour,
			Now:       func() time.Time { return time.Date(2026, 8, 9, 3, 0, 0, 0, time.UTC) },
		},
		Modes: CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{4, 6}},
	}
}

type testCodexLeaseOwner struct {
	err error
}

type countingCodexLeaseTestOwner struct {
	asserts int
	begins  int
}

type mutatingCodexLeaseTestOwner struct {
	mutate func()
}

func (owner mutatingCodexLeaseTestOwner) AssertOwner() error { return nil }

func (owner mutatingCodexLeaseTestOwner) BeginOwnerOperation() (*codex.CredentialOwnerOperation, error) {
	owner.mutate()
	return &codex.CredentialOwnerOperation{}, nil
}

func (owner *countingCodexLeaseTestOwner) AssertOwner() error {
	owner.asserts++
	return nil
}

func (owner *countingCodexLeaseTestOwner) BeginOwnerOperation() (*codex.CredentialOwnerOperation, error) {
	owner.begins++
	return &codex.CredentialOwnerOperation{}, nil
}

func (owner testCodexLeaseOwner) AssertOwner() error { return owner.err }

func (owner testCodexLeaseOwner) BeginOwnerOperation() (*codex.CredentialOwnerOperation, error) {
	if owner.err != nil {
		return nil, owner.err
	}
	return &codex.CredentialOwnerOperation{}, nil
}

type testCodexLeaseV1Record struct {
	SessionHash       string     `json:"session_hash"`
	ThreadHash        string     `json:"thread_hash"`
	TurnHash          string     `json:"turn_hash"`
	NamespaceHash     string     `json:"namespace_hash"`
	AccountHash       string     `json:"account_hash"`
	CorrelationHash   string     `json:"correlation_hash,omitempty"`
	TurnStateHash     string     `json:"turn_state_hash,omitempty"`
	State             LeaseState `json:"state"`
	LeaseGeneration   uint64     `json:"lease_generation"`
	ModeEpoch         uint64     `json:"mode_epoch"`
	Authoritative     bool       `json:"authoritative"`
	ActiveRefs        int        `json:"active_refs,omitempty"`
	SocketGeneration  uint64     `json:"socket_generation,omitempty"`
	HasEncryptedState bool       `json:"has_encrypted_state,omitempty"`
	HasResponseAnchor bool       `json:"has_response_anchor,omitempty"`
	HasTurnState      bool       `json:"has_turn_state,omitempty"`
	NonMigratable     bool       `json:"non_migratable,omitempty"`
	LastSeen          time.Time  `json:"last_seen"`
}

type testCodexLeaseV1Envelope struct {
	Version    int                      `json:"version"`
	Generation uint64                   `json:"generation"`
	Records    []testCodexLeaseV1Record `json:"records"`
	MAC        string                   `json:"mac"`
}

func writeCodexLeaseV1Fixture(t *testing.T, fsys *fsutil.MemFS) ([]byte, []byte) {
	t.Helper()
	if err := fsys.MkdirAll("/state", 0o700); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0x5a}, 32)
	if err := fsys.WriteFile("/state/leases.key", key, 0o600); err != nil {
		t.Fatal(err)
	}
	hash := func(domain, value string) string {
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write([]byte(domain))
		_, _ = mac.Write([]byte{0})
		_, _ = mac.Write([]byte(value))
		return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	}
	base := testCodexLeaseV1Record{
		SessionHash:       hash("session", "session-one"),
		ThreadHash:        hash("thread", "thread-one"),
		TurnHash:          hash("turn", "turn-one"),
		NamespaceHash:     hash("namespace", CodexResponsesNamespace),
		AccountHash:       hash("account", "account-one"),
		CorrelationHash:   hash("correlation", "response-one"),
		TurnStateHash:     hash("turn-state", "state-one"),
		State:             LeaseBoundQuiescent,
		LeaseGeneration:   9,
		ActiveRefs:        0,
		SocketGeneration:  17,
		HasEncryptedState: true,
		HasResponseAnchor: true,
		HasTurnState:      true,
		LastSeen:          time.Date(2026, 8, 8, 1, 2, 3, 4, time.UTC),
	}
	authorityFour := base
	authorityFour.ModeEpoch = 4
	authorityFour.Authoritative = true
	shadowFive := base
	shadowFive.SessionHash = hash("session", "session-two")
	shadowFive.ThreadHash = hash("thread", "thread-two")
	shadowFive.TurnHash = hash("turn", "turn-shadow")
	shadowFive.ModeEpoch = 5
	shadowFive.Authoritative = false
	authoritySix := base
	authoritySix.TurnHash = hash("turn", "turn-two")
	authoritySix.ModeEpoch = 6
	authoritySix.Authoritative = true
	envelope := testCodexLeaseV1Envelope{
		Version:    1,
		Generation: 7,
		Records:    []testCodexLeaseV1Record{authoritySix, shadowFive, authorityFour},
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	envelope.MAC = base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	data, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("/state/leases.json", data, 0o600); err != nil {
		t.Fatal(err)
	}
	return key, data
}

func validCodexLeaseV2TestMAC(key []byte, envelope codexLeaseJournalEnvelopeV2) bool {
	got := envelope.MAC
	envelope.MAC = ""
	payload, err := json.Marshal(envelope)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	want, wantOK := decodeCanonicalCodexLeaseMAC(base64.RawURLEncoding.EncodeToString(mac.Sum(nil)))
	gotBytes, gotOK := decodeCanonicalCodexLeaseMAC(got)
	return wantOK && gotOK && hmac.Equal(gotBytes, want)
}
