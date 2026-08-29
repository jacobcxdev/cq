//go:build windows

package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsSecureDirectoryTransferRoundTrip(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	fys := newWindowsTestFileSystem(t, root)
	if err := EnsureSecureDirectory(fys, state); err != nil {
		t.Fatal(err)
	}
	source, err := fys.OpenSecureDirectory(state)
	if err != nil {
		t.Fatal(err)
	}
	handle, grant, err := DuplicateSecureDirectoryIntoProcess(source, windows.CurrentProcess(), uint32(os.Getpid()))
	if err != nil {
		_ = source.Close()
		t.Fatal(err)
	}
	encoded, err := grant.MarshalText()
	if err != nil {
		_ = windows.CloseHandle(handle)
		_ = source.Close()
		t.Fatal(err)
	}
	var decoded SecureDirectoryTransferGrantV1
	if err := decoded.UnmarshalText(encoded); err != nil {
		_ = windows.CloseHandle(handle)
		_ = source.Close()
		t.Fatal(err)
	}
	if decoded != grant {
		_ = windows.CloseHandle(handle)
		_ = source.Close()
		t.Fatalf("decoded grant differs: %#v != %#v", decoded, grant)
	}
	if err := source.Close(); err != nil {
		_ = windows.CloseHandle(handle)
		t.Fatal(err)
	}
	adopted, err := AdoptTransferredSecureDirectory(handle, decoded)
	if err != nil {
		t.Fatal(err)
	}
	file, err := adopted.CreateExclusive("journal", 0o600)
	if err != nil {
		_ = adopted.Close()
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("transferred")); err != nil {
		_ = file.Close()
		_ = adopted.Close()
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = adopted.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		_ = adopted.Close()
		t.Fatal(err)
	}
	if err := adopted.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(state, "journal"))
	if err != nil || string(data) != "transferred" {
		t.Fatalf("transferred journal = %q, %v", data, err)
	}
}

func TestWindowsSecureDirectoryTransferRejectsMalformedGrant(t *testing.T) {
	zero := SecureDirectoryTransferGrantV1{}
	if _, err := zero.MarshalText(); !errors.Is(err, ErrUnsafeSecurePath) {
		t.Fatalf("zero grant error = %v", err)
	}
	valid := windowsSecureDirectoryTransferDomain + ":0000000000000001:00000000000000000000000000000001:0000000000000000:0000000000000001:" + strings.Repeat("01", 32)
	invalid := []string{
		"",
		valid + "00",
		strings.ToUpper(valid),
		strings.Replace(valid, windowsSecureDirectoryTransferDomain, "cq/fsutil-secure-directory-transfer/v2", 1),
		strings.Replace(valid, ":0000000000000001:", ":0000000000000000:", 1),
		strings.Replace(valid, ":0000000000000000:0000000000000001:", ":0000000000000001:0000000000000001:", 1),
	}
	for _, encoded := range invalid {
		var grant SecureDirectoryTransferGrantV1
		if err := grant.UnmarshalText([]byte(encoded)); !errors.Is(err, ErrUnsafeSecurePath) {
			t.Fatalf("grant %q error = %v", encoded, err)
		}
	}
}

func TestWindowsSecureDirectoryTransferRejectsGrantAndDACLDrift(t *testing.T) {
	for _, drift := range []string{"grant", "DACL"} {
		t.Run(drift, func(t *testing.T) {
			root := t.TempDir()
			state := filepath.Join(root, "state")
			fys := newWindowsTestFileSystem(t, root)
			if err := EnsureSecureDirectory(fys, state); err != nil {
				t.Fatal(err)
			}
			source, err := fys.OpenSecureDirectory(state)
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			handle, grant, err := DuplicateSecureDirectoryIntoProcess(source, windows.CurrentProcess(), uint32(os.Getpid()))
			if err != nil {
				t.Fatal(err)
			}
			if drift == "grant" {
				grant.security[0] ^= 1
			} else {
				setWindowsTestDACL(t, state, "D:P(A;;FA;;;WD)", true)
			}
			if directory, err := AdoptTransferredSecureDirectory(handle, grant); !errors.Is(err, ErrUnsafeSecurePath) {
				if directory != nil {
					_ = directory.Close()
				}
				t.Fatalf("%s drift error = %v", drift, err)
			}
			if err := windows.CloseHandle(handle); err == nil {
				t.Fatal("rejected transferred handle remained open")
			}
		})
	}
}

func TestWindowsSecureDirectoryTransferRejectsInheritableHandleAndWrongSource(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	fys := newWindowsTestFileSystem(t, root)
	if err := EnsureSecureDirectory(fys, state); err != nil {
		t.Fatal(err)
	}
	source, err := fys.OpenSecureDirectory(state)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	transient, grant, err := DuplicateSecureDirectoryIntoProcess(source, windows.CurrentProcess(), uint32(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.CloseHandle(transient); err != nil {
		t.Fatal(err)
	}
	opened := source.(*windowsSecureDirectory)
	var inheritable windows.Handle
	if err := windows.DuplicateHandle(windows.CurrentProcess(), windows.Handle(opened.file.Fd()), windows.CurrentProcess(), &inheritable, 0, true, windows.DUPLICATE_SAME_ACCESS); err != nil {
		t.Fatal(err)
	}
	if directory, err := AdoptTransferredSecureDirectory(inheritable, grant); !errors.Is(err, ErrUnsafeSecurePath) {
		if directory != nil {
			_ = directory.Close()
		}
		t.Fatalf("inheritable handle error = %v", err)
	}
	mem := NewMemFS()
	if err := EnsureSecureDirectory(mem, "/state"); err != nil {
		t.Fatal(err)
	}
	wrong, err := mem.OpenSecureDirectory("/state")
	if err != nil {
		t.Fatal(err)
	}
	defer wrong.Close()
	if handle, _, err := DuplicateSecureDirectoryIntoProcess(wrong, windows.CurrentProcess(), uint32(os.Getpid())); handle != 0 || !errors.Is(err, ErrSecureCapabilityUnavailable) {
		t.Fatalf("wrong source = %v, %v", handle, err)
	}
	if _, _, err := DuplicateSecureDirectoryIntoProcess(source, windows.CurrentProcess(), uint32(os.Getpid()+1)); !errors.Is(err, ErrUnsafeSecurePath) {
		t.Fatalf("wrong PID error = %v", err)
	}
}
