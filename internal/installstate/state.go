package installstate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

const (
	CurrentSchemaVersion = 1
	maxInstallStateBytes = 64 << 10
)

var (
	ErrNotInstalled      = errors.New("CQ is not installed")
	ErrUnknownSchema     = errors.New("unknown installation state schema")
	ErrInvalidRecord     = errors.New("invalid installation state")
	ErrOwnershipConflict = errors.New("installation ownership conflict")
)

type Owner string

const (
	OwnerManual   Owner = "manual"
	OwnerHomebrew Owner = "homebrew"
	OwnerWinGet   Owner = "winget"
	OwnerGo       Owner = "go"
)

func (owner Owner) Valid() bool {
	switch owner {
	case OwnerManual, OwnerHomebrew, OwnerWinGet, OwnerGo:
		return true
	default:
		return false
	}
}

type Record struct {
	SchemaVersion int      `json:"schema_version"`
	Owner         Owner    `json:"owner"`
	Version       string   `json:"version"`
	Executable    string   `json:"executable"`
	BinaryDigest  string   `json:"binary_digest"`
	Services      []string `json:"services"`
}

type Store struct {
	FS    fsutil.FileSystem
	Roots userdirs.Roots
}

func (store Store) Path() string {
	return filepath.Join(store.Roots.State, "install.json")
}

func (store Store) Load() (Record, error) {
	if err := store.validate(); err != nil {
		return Record{}, err
	}
	data, err := fsutil.ReadSecureFile(store.FS, store.Path(), maxInstallStateBytes)
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, ErrNotInstalled
	}
	if err != nil {
		return Record{}, fmt.Errorf("read installation state: %w", err)
	}

	var record Record
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return Record{}, fmt.Errorf("%w: decode JSON: %v", ErrInvalidRecord, err)
	}
	if err := requireJSONEnd(decoder); err != nil {
		return Record{}, fmt.Errorf("%w: %v", ErrInvalidRecord, err)
	}
	if record.SchemaVersion != CurrentSchemaVersion {
		return Record{}, fmt.Errorf("%w: got %d, support %d", ErrUnknownSchema, record.SchemaVersion, CurrentSchemaVersion)
	}
	if err := record.Validate(); err != nil {
		return Record{}, err
	}
	return record, nil
}

func (store Store) Save(record Record) error {
	if err := store.validate(); err != nil {
		return err
	}
	if err := record.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encode installation state: %w", err)
	}
	data = append(data, '\n')
	if err := fsutil.SecureAtomicWrite(store.FS, store.Path(), data); err != nil {
		return fmt.Errorf("commit installation state: %w", err)
	}
	return nil
}

func (store Store) Remove() error {
	if err := store.validate(); err != nil {
		return err
	}
	if _, err := store.Load(); errors.Is(err, ErrNotInstalled) {
		return nil
	} else if err != nil {
		return err
	}
	if err := store.FS.Remove(store.Path()); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("remove installation state: %w", err)
	}
	if durable, ok := store.FS.(fsutil.DurableFileSystem); ok {
		if err := durable.SyncDir(filepath.Dir(store.Path())); err != nil {
			return fmt.Errorf("sync installation state directory: %w", err)
		}
	}
	return nil
}

func (store Store) CheckClaim(owner Owner, executable string) error {
	claim := Record{
		SchemaVersion: CurrentSchemaVersion,
		Owner:         owner,
		Version:       "claim",
		Executable:    executable,
		BinaryDigest:  strings.Repeat("0", sha256.Size*2),
		Services:      []string{"proxy", "refresh"},
	}
	if err := claim.Validate(); err != nil {
		return err
	}
	existing, err := store.Load()
	if errors.Is(err, ErrNotInstalled) {
		return nil
	}
	if err != nil {
		return err
	}
	if existing.Owner == owner && existing.Executable == executable {
		return nil
	}
	return fmt.Errorf(
		"%w: existing owner %q executable %q; requested owner %q executable %q",
		ErrOwnershipConflict,
		existing.Owner,
		existing.Executable,
		owner,
		executable,
	)
}

func (record Record) Validate() error {
	if record.SchemaVersion != CurrentSchemaVersion {
		return fmt.Errorf("%w: schema version %d", ErrInvalidRecord, record.SchemaVersion)
	}
	if !record.Owner.Valid() {
		return fmt.Errorf("%w: owner %q", ErrInvalidRecord, record.Owner)
	}
	if strings.TrimSpace(record.Version) == "" {
		return fmt.Errorf("%w: empty version", ErrInvalidRecord)
	}
	if record.Executable == "" || !filepath.IsAbs(record.Executable) || filepath.Clean(record.Executable) != record.Executable {
		return fmt.Errorf("%w: executable must be a clean absolute path", ErrInvalidRecord)
	}
	if !validDigest(record.BinaryDigest) {
		return fmt.Errorf("%w: invalid binary digest", ErrInvalidRecord)
	}
	if len(record.Services) == 0 {
		return fmt.Errorf("%w: no services", ErrInvalidRecord)
	}
	seen := make(map[string]struct{}, len(record.Services))
	for _, service := range record.Services {
		if strings.TrimSpace(service) == "" || service != strings.TrimSpace(service) {
			return fmt.Errorf("%w: invalid service ID %q", ErrInvalidRecord, service)
		}
		if _, exists := seen[service]; exists {
			return fmt.Errorf("%w: duplicate service ID %q", ErrInvalidRecord, service)
		}
		seen[service] = struct{}{}
	}
	return nil
}

// DigestFile returns the SHA-256 digest of one bounded regular executable.
func DigestFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > 512<<20 {
		return "", fmt.Errorf("invalid executable file")
	}
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, (512<<20)+1))
	if err != nil {
		return "", err
	}
	if written != info.Size() {
		return "", fmt.Errorf("executable changed while hashing")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func validDigest(digest string) bool {
	if len(digest) != sha256.Size*2 {
		return false
	}
	for _, character := range digest {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}

func (store Store) validate() error {
	if store.FS == nil || store.Roots.State == "" || !filepath.IsAbs(store.Roots.State) || filepath.Clean(store.Roots.State) != store.Roots.State {
		return fmt.Errorf("%w: invalid state store", ErrInvalidRecord)
	}
	return nil
}

func requireJSONEnd(decoder *json.Decoder) error {
	var extra json.RawMessage
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return fmt.Errorf("multiple JSON values")
}
