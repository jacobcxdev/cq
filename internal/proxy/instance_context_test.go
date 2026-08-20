package proxy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestInstanceContextOpensValidatedRootAndExpectedIdentity(t *testing.T) {
	root := makeInstanceRoot(t, "candidate")
	reader := staticInstanceIdentityReader{identity: ProxyInstanceIdentity{ProxyInstanceID: "candidate-1", Role: ProxyInstanceCandidate}}
	instance, err := OpenInstanceContext(root, "candidate-1", WithInstanceIdentityReader(reader))
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	if instance.Root != root || instance.Basename != "candidate" || instance.Identity.ProxyInstanceID != "candidate-1" {
		t.Fatalf("context = %#v", instance)
	}
}

func TestInstanceContextRejectsTraversalSymlinkAndUnsafeMode(t *testing.T) {
	root := makeInstanceRoot(t, "instance")
	reader := staticInstanceIdentityReader{identity: ProxyInstanceIdentity{ProxyInstanceID: "instance-1", Role: ProxyInstancePrimary}}
	if _, err := OpenInstanceContext(root+string(filepath.Separator)+".."+string(filepath.Separator)+"instance", "instance-1", WithInstanceIdentityReader(reader)); err == nil {
		t.Fatal("lexically unclean root accepted")
	}
	link := filepath.Join(filepath.Dir(root), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenInstanceContext(link, "instance-1", WithInstanceIdentityReader(reader)); err == nil {
		t.Fatal("symlink root accepted")
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenInstanceContext(root, "instance-1", WithInstanceIdentityReader(reader)); err == nil {
		t.Fatal("unsafe root mode accepted")
	}
}

func TestInstanceContextRejectsIdentityAndPrimaryCandidateConfusion(t *testing.T) {
	root := makeInstanceRoot(t, "default")
	reader := staticInstanceIdentityReader{identity: ProxyInstanceIdentity{ProxyInstanceID: "candidate-1", Role: ProxyInstanceCandidate}}
	if _, err := OpenInstanceContext(root, "other", WithInstanceIdentityReader(reader)); !errors.Is(err, ErrProxyInstanceMismatch) {
		t.Fatalf("identity mismatch error = %v", err)
	}
	if _, err := OpenInstanceContext(root, "candidate-1", WithInstanceIdentityReader(reader), WithReservedPrimaryRoot(root)); !errors.Is(err, ErrProxyInstanceRoleConfusion) {
		t.Fatalf("role confusion error = %v", err)
	}
}

func TestInstanceContextCandidateAllowsAbsentReservedPrimaryChild(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	candidateRoot := filepath.Join(parent, "candidate")
	if err := os.Mkdir(candidateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	reservedRoot := filepath.Join(parent, "default")
	reader := staticInstanceIdentityReader{identity: ProxyInstanceIdentity{ProxyInstanceID: "candidate-1", Role: ProxyInstanceCandidate}}
	instance, err := OpenInstanceContext(candidateRoot, "candidate-1", WithInstanceIdentityReader(reader), WithReservedPrimaryRoot(reservedRoot))
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	if _, err := os.Lstat(reservedRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reserved primary child was created: %v", err)
	}
}

func TestInstanceContextRejectsReservedPrimaryAlias(t *testing.T) {
	base := t.TempDir()
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(base, "instances")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "default")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(filepath.Dir(base), filepath.Base(base)+"-alias")
	if err := os.Symlink(base, alias); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(alias) })
	reservedAlias := filepath.Join(alias, "instances", "default")
	reader := staticInstanceIdentityReader{identity: ProxyInstanceIdentity{ProxyInstanceID: "candidate-1", Role: ProxyInstanceCandidate}}
	if _, err := OpenInstanceContext(root, "candidate-1", WithInstanceIdentityReader(reader), WithReservedPrimaryRoot(reservedAlias)); !errors.Is(err, ErrProxyInstanceRoleConfusion) {
		t.Fatalf("reserved alias error = %v", err)
	}
}

func TestInstanceContextCloseReleasesRetainedCapabilities(t *testing.T) {
	filesystem := fsutil.NewMemFS()
	if err := filesystem.MkdirAll("/parent/root", 0o700); err != nil {
		t.Fatal(err)
	}
	reader := staticInstanceIdentityReader{identity: ProxyInstanceIdentity{ProxyInstanceID: "instance-1", Role: ProxyInstancePrimary}}
	instance, err := OpenInstanceContext("/parent/root", "instance-1", WithInstanceFileSystem(filesystem), WithInstanceIdentityReader(reader))
	if err != nil {
		t.Fatal(err)
	}
	if err := instance.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := instance.RootDirectory.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("root descriptor remains usable: %v", err)
	}
}

func TestInstanceContextRejectsReplacedParent(t *testing.T) {
	root := makeInstanceRoot(t, "instance")
	filesystem := &replacingInstanceFileSystem{parent: filepath.Dir(root)}
	reader := staticInstanceIdentityReader{identity: ProxyInstanceIdentity{ProxyInstanceID: "instance-1", Role: ProxyInstancePrimary}}
	if _, err := OpenInstanceContext(root, "instance-1", WithInstanceFileSystem(filesystem), WithInstanceIdentityReader(reader)); !errors.Is(err, fsutil.ErrUnsafeSecurePath) {
		t.Fatalf("replaced parent error = %v", err)
	}
}

type staticInstanceIdentityReader struct {
	identity ProxyInstanceIdentity
	err      error
}

type replacingInstanceFileSystem struct {
	fsutil.OSFileSystem
	parent string
	opens  int
}

func (filesystem *replacingInstanceFileSystem) OpenSecureDirectory(name string) (fsutil.SecureDirectory, error) {
	filesystem.opens++
	if filesystem.opens == 2 && name == filesystem.parent {
		replaced := filesystem.parent + ".replaced"
		if err := os.Rename(filesystem.parent, replaced); err != nil {
			return nil, err
		}
		if err := os.Mkdir(filesystem.parent, 0o700); err != nil {
			return nil, err
		}
		opened, err := filesystem.OSFileSystem.OpenSecureDirectory(name)
		if err != nil {
			return nil, err
		}
		if err := os.Remove(filesystem.parent); err != nil {
			_ = opened.Close()
			return nil, err
		}
		if err := os.Rename(replaced, filesystem.parent); err != nil {
			_ = opened.Close()
			return nil, err
		}
		return opened, nil
	}
	return filesystem.OSFileSystem.OpenSecureDirectory(name)
}

func (reader staticInstanceIdentityReader) ReadProxyInstanceIdentity(_ context.Context, _ fsutil.SecureDirectory) (ProxyInstanceIdentity, error) {
	return reader.identity, reader.err
}

func makeInstanceRoot(t *testing.T, basename string) string {
	t.Helper()
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, basename)
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}
