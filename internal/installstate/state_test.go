package installstate

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func TestStoreRoundTrip(t *testing.T) {
	stateRoot := newStateRoot(t)
	store := Store{FS: fsutil.OSFileSystem{}, Roots: userdirs.Roots{State: stateRoot}}
	want := validRecord(filepath.Join(stateRoot, "bin", executableName()))

	if err := store.Save(want); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Load() = %#v, want %#v", got, want)
	}
	info, err := os.Stat(store.Path())
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("install state mode = %o, want 600", got)
	}
}

func TestStoreLoadReportsMissingInstallation(t *testing.T) {
	store := Store{FS: fsutil.OSFileSystem{}, Roots: userdirs.Roots{State: newStateRoot(t)}}

	_, err := store.Load()
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("Load() error = %v, want ErrNotInstalled", err)
	}
}

func TestStoreRejectsUnknownSchema(t *testing.T) {
	store := Store{FS: fsutil.OSFileSystem{}, Roots: userdirs.Roots{State: newStateRoot(t)}}
	writeInstallState(t, store, `{"schema_version":2,"owner":"go","version":"0.27.0","executable":"/tmp/cq","services":["proxy","refresh"]}`)

	_, err := store.Load()
	if !errors.Is(err, ErrUnknownSchema) {
		t.Fatalf("Load() error = %v, want ErrUnknownSchema", err)
	}
}

func TestStoreRejectsUnknownJSONFields(t *testing.T) {
	store := Store{FS: fsutil.OSFileSystem{}, Roots: userdirs.Roots{State: newStateRoot(t)}}
	writeInstallState(t, store, `{"schema_version":1,"owner":"go","version":"0.27.0","executable":"/tmp/cq","services":["proxy","refresh"],"unexpected":true}`)

	_, err := store.Load()
	if !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("Load() error = %v, want ErrInvalidRecord", err)
	}
}

func TestStoreRejectsInvalidRecords(t *testing.T) {
	stateRoot := newStateRoot(t)
	valid := validRecord(filepath.Join(stateRoot, "bin", executableName()))
	tests := []struct {
		name   string
		mutate func(*Record)
	}{
		{name: "schema", mutate: func(record *Record) { record.SchemaVersion = 0 }},
		{name: "owner", mutate: func(record *Record) { record.Owner = Owner("package") }},
		{name: "version", mutate: func(record *Record) { record.Version = "" }},
		{name: "relative executable", mutate: func(record *Record) { record.Executable = "bin/cq" }},
		{name: "unclean executable", mutate: func(record *Record) {
			record.Executable = stateRoot + string(filepath.Separator) + "bin" + string(filepath.Separator) + ".." + string(filepath.Separator) + "cq"
		}},
		{name: "missing services", mutate: func(record *Record) { record.Services = nil }},
		{name: "empty service", mutate: func(record *Record) { record.Services[0] = "" }},
		{name: "duplicate service", mutate: func(record *Record) { record.Services[1] = record.Services[0] }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record := valid
			record.Services = append([]string(nil), valid.Services...)
			test.mutate(&record)
			store := Store{FS: fsutil.OSFileSystem{}, Roots: userdirs.Roots{State: filepath.Join(stateRoot, test.name)}}

			if err := store.Save(record); !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("Save() error = %v, want ErrInvalidRecord", err)
			}
			if _, err := os.Stat(store.Path()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid record created state file: %v", err)
			}
		})
	}
}

func TestStoreCheckClaim(t *testing.T) {
	stateRoot := newStateRoot(t)
	executable := filepath.Join(stateRoot, "bin", executableName())
	store := Store{FS: fsutil.OSFileSystem{}, Roots: userdirs.Roots{State: stateRoot}}

	if err := store.CheckClaim(OwnerGo, executable); err != nil {
		t.Fatalf("CheckClaim() absent error = %v", err)
	}
	if err := store.Save(validRecord(executable)); err != nil {
		t.Fatal(err)
	}
	if err := store.CheckClaim(OwnerGo, executable); err != nil {
		t.Fatalf("CheckClaim() same claim error = %v", err)
	}
	if err := store.CheckClaim(OwnerHomebrew, executable); !errors.Is(err, ErrOwnershipConflict) {
		t.Fatalf("CheckClaim() owner error = %v, want ErrOwnershipConflict", err)
	}
	if err := store.CheckClaim(OwnerGo, filepath.Join(stateRoot, "other", executableName())); !errors.Is(err, ErrOwnershipConflict) {
		t.Fatalf("CheckClaim() path error = %v, want ErrOwnershipConflict", err)
	}
}

func TestStoreRemoveIsIdempotent(t *testing.T) {
	stateRoot := newStateRoot(t)
	store := Store{FS: fsutil.OSFileSystem{}, Roots: userdirs.Roots{State: stateRoot}}
	if err := store.Save(validRecord(filepath.Join(stateRoot, executableName()))); err != nil {
		t.Fatal(err)
	}

	if err := store.Remove(); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if err := store.Remove(); err != nil {
		t.Fatalf("Remove() repeated error = %v", err)
	}
	if _, err := store.Load(); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("Load() after remove error = %v, want ErrNotInstalled", err)
	}
}

func TestStoreSaveFailsClosedWithoutSecureFilesystem(t *testing.T) {
	stateRoot := newStateRoot(t)
	store := Store{
		FS:    fileSystemOnly{FileSystem: fsutil.OSFileSystem{}},
		Roots: userdirs.Roots{State: stateRoot},
	}

	err := store.Save(validRecord(filepath.Join(stateRoot, executableName())))
	if !errors.Is(err, fsutil.ErrSecureCapabilityUnavailable) {
		t.Fatalf("Save() error = %v, want secure capability error", err)
	}
	if _, err := os.Stat(store.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed save created state file: %v", err)
	}
}

func validRecord(executable string) Record {
	return Record{
		SchemaVersion: CurrentSchemaVersion,
		Owner:         OwnerGo,
		Version:       "0.27.0",
		Executable:    executable,
		Services:      []string{"proxy", "refresh"},
	}
}

func executableName() string {
	if filepath.Separator == '\\' {
		return "cq.exe"
	}
	return "cq"
}

func newStateRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "state")
}

func writeInstallState(t *testing.T, store Store, data string) {
	t.Helper()
	if err := fsutil.SecureAtomicWrite(store.FS, store.Path(), []byte(data)); err != nil {
		t.Fatalf("write raw install state: %v", err)
	}
}

type fileSystemOnly struct {
	fsutil.FileSystem
}
