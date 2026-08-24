package proxy

import (
	"context"
	"os"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

type principalOnlyMemFS struct {
	*fsutil.MemFS
	principal fsutil.SecurePrincipal
}

func newPrincipalOnlyMemFS() *principalOnlyMemFS {
	return &principalOnlyMemFS{
		MemFS: fsutil.NewMemFS(),
		principal: fsutil.SecurePrincipal{
			Kind: fsutil.SecurePrincipalSID, SIDLength: 4, SID: [68]byte{1, 2, 3, 4},
		},
	}
}

func (*principalOnlyMemFS) EffectiveUID() uint64                    { return 0 }
func (*principalOnlyMemFS) FileOwnerUID(os.FileInfo) (uint64, bool) { return 0, false }

func (fsys *principalOnlyMemFS) EffectivePrincipal() (fsutil.SecurePrincipal, bool) {
	return fsys.principal, true
}

func (fsys *principalOnlyMemFS) FileOwnerPrincipal(os.FileInfo) (fsutil.SecurePrincipal, bool) {
	return fsys.principal, true
}

func TestPrivateConsumersAcceptPrincipalInspectionWithoutUID(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T, *principalOnlyMemFS, fsutil.SecureDirectory)
	}{
		{name: "authority", run: func(t *testing.T, fsys *principalOnlyMemFS, directory fsutil.SecureDirectory) {
			if _, err := authorityDirectoryIdentity(fsys, directory); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "instance", run: func(t *testing.T, fsys *principalOnlyMemFS, directory fsutil.SecureDirectory) {
			if _, err := validateInstanceDirectory(fsys, directory); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "readiness", run: func(t *testing.T, fsys *principalOnlyMemFS, directory fsutil.SecureDirectory) {
			if err := validateCodexReadinessMarkerDirectoryAuthority(fsys, directory, "/state"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "canary_directory", run: func(t *testing.T, fsys *principalOnlyMemFS, directory fsutil.SecureDirectory) {
			if err := validateCodexCanaryRetainedDirectory(fsys, fsys, directory, "/state"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "canary_owner", run: func(t *testing.T, fsys *principalOnlyMemFS, directory fsutil.SecureDirectory) {
			lock, err := fsutil.AcquireExclusiveLockInDirectory(fsys, directory, codexCanaryOwnerLockName)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			recorder := &CodexCanaryRecorder{ownerLock: lock, ownerDirectory: directory, ownerInspector: fsys}
			if err := recorder.requireOwnerLocked(); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "operation_occupancy", run: func(t *testing.T, fsys *principalOnlyMemFS, directory fsutil.SecureDirectory) {
			if err := fsys.WriteFile("/state/object", []byte("value"), 0o600); err != nil {
				t.Fatal(err)
			}
			backend := &CredentialAuthorityFSBackend{inspector: fsys, directory: directory}
			occupancy, err := backend.CredentialAuthorityOccupancy(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if occupancy.Files != 1 || occupancy.Bytes != 5 {
				t.Fatalf("occupancy = %#v", occupancy)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fsys := newPrincipalOnlyMemFS()
			if err := fsys.MkdirAll("/state", 0o700); err != nil {
				t.Fatal(err)
			}
			directory, err := fsys.OpenSecureDirectory("/state")
			if err != nil {
				t.Fatal(err)
			}
			defer directory.Close()
			test.run(t, fsys, directory)
		})
	}
}
