//go:build windows

package fsutil

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unsafe"

	"github.com/jacobcxdev/cq/internal/userdirs"
	"golang.org/x/sys/windows"
)

type fixedWindowsBoundaryResolver struct {
	anchorPath     string
	anchorIdentity SecureFileIdentity
}

func (resolver *fixedWindowsBoundaryResolver) ResolveSecureBoundary(path string, purpose secureBoundaryPurpose) (secureBoundarySelection, error) {
	clean, err := validateWindowsAbsolutePath(path)
	if err != nil || !windowsPathWithin(resolver.anchorPath, clean) {
		return secureBoundarySelection{}, fmt.Errorf("%w: Windows test boundary", ErrUnsafeSecurePath)
	}
	return secureBoundarySelection{
		AnchorPath:        resolver.anchorPath,
		PostAnchorPrivate: purpose == secureBoundaryCQPrivate,
	}, nil
}

func (resolver *fixedWindowsBoundaryResolver) SecureBoundaryIdentity() (SecureFileIdentity, bool) {
	return resolver.anchorIdentity, resolver.anchorIdentity != (SecureFileIdentity{})
}

func newWindowsTestFileSystem(t *testing.T, anchor string) OSFileSystem {
	t.Helper()
	absolute, err := filepath.Abs(anchor)
	if err != nil {
		t.Fatal(err)
	}
	resolver := &fixedWindowsBoundaryResolver{anchorPath: filepath.Clean(absolute)}
	fsys := OSFileSystem{secureBoundaryResolver: resolver}
	info, err := fsys.Lstat(resolver.anchorPath)
	if err != nil {
		t.Fatal(err)
	}
	identity, ok := fsys.FileIdentity(info)
	if !ok || identity.Device == 0 || identity.FileID == ([16]byte{}) {
		t.Fatalf("anchor identity = %#v, %v", identity, ok)
	}
	resolver.anchorIdentity = identity
	return fsys
}

func TestWindowsSecureAtomicWriteStaysInRetainedDirectory(t *testing.T) {
	root := t.TempDir()
	canonical := filepath.Join(root, "state")
	moved := filepath.Join(root, "state-opened")
	fsys := newWindowsTestFileSystem(t, root)
	if err := EnsureSecureDirectory(fsys, canonical); err != nil {
		t.Fatal(err)
	}
	directory, err := fsys.OpenSecureDirectory(canonical)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if err := os.Rename(canonical, moved); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSecureDirectory(fsys, canonical); err != nil {
		t.Fatal(err)
	}
	if err := SecureAtomicWriteInDirectory(fsys, directory, "value", []byte("retained")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(moved, "value"))
	if err != nil || string(got) != "retained" {
		t.Fatalf("retained value = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(canonical, "value")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement namespace received value: %v", err)
	}
}

func TestWindowsSecureAtomicCreateHasOneWinner(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	fsys := newWindowsTestFileSystem(t, root)
	if err := EnsureSecureDirectory(fsys, state); err != nil {
		t.Fatal(err)
	}
	directory, err := fsys.OpenSecureDirectory(state)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	start := make(chan struct{})
	errorsByValue := make([]error, 2)
	var wait sync.WaitGroup
	for index, value := range []string{"one", "two"} {
		wait.Add(1)
		go func(index int, value string) {
			defer wait.Done()
			<-start
			errorsByValue[index] = SecureAtomicCreateInDirectory(fsys, directory, "canonical", []byte(value))
		}(index, value)
	}
	close(start)
	wait.Wait()
	winners, losers := 0, 0
	for _, err := range errorsByValue {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, os.ErrExist) && errors.Is(err, ErrCommitNotCommitted):
			losers++
		default:
			t.Fatalf("rename error = %v", err)
		}
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("rename winners/losers = %d/%d, want 1/1", winners, losers)
	}
	got, err := os.ReadFile(filepath.Join(state, "canonical"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "one" && string(got) != "two" {
		t.Fatalf("canonical content = %q", got)
	}
}

func TestWindowsSecureAtomicWriteRejectsDACLDrift(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	canonical := filepath.Join(state, "value")
	fys := newWindowsTestFileSystem(t, root)
	if err := EnsureSecureDirectory(fys, state); err != nil {
		t.Fatal(err)
	}
	directory, err := fys.OpenSecureDirectory(state)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if err := SecureAtomicWriteInDirectory(fys, directory, "value", []byte("old")); err != nil {
		t.Fatal(err)
	}
	err = SecureAtomicWriteInDirectoryChecked(fys, directory, "value", []byte("new"), func() error {
		setWindowsTestDACL(t, canonical, "D:P(A;;FA;;;WD)", true)
		return ValidateSecureRegularFile(fys, canonical)
	})
	if !errors.Is(err, ErrCommitNotCommitted) || !errors.Is(err, ErrUnsafeSecurePath) {
		t.Fatalf("DACL drift error = %v", err)
	}
	data, readErr := os.ReadFile(canonical)
	if readErr != nil || string(data) != "old" {
		t.Fatalf("canonical content = %q, %v", data, readErr)
	}
}

func TestWindowsSecureAtomicRejectsTemporaryReplacement(t *testing.T) {
	for _, noReplace := range []bool{false, true} {
		name := "write"
		if noReplace {
			name = "create"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			state := filepath.Join(root, "state")
			fys := newWindowsTestFileSystem(t, root)
			if err := EnsureSecureDirectory(fys, state); err != nil {
				t.Fatal(err)
			}
			opened, err := fys.OpenSecureDirectory(state)
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close()
			if !noReplace {
				if err := SecureAtomicWriteInDirectory(fys, opened, "value", []byte("old")); err != nil {
					t.Fatal(err)
				}
			}
			directory := &windowsReplacingTemporaryDirectory{SecureDirectory: opened}
			if noReplace {
				err = SecureAtomicCreateInDirectory(fys, directory, "value", []byte("new"))
			} else {
				err = SecureAtomicWriteInDirectory(fys, directory, "value", []byte("new"))
			}
			if !errors.Is(err, ErrCommitNotCommitted) || !errors.Is(err, ErrUnsafeSecurePath) {
				t.Fatalf("temporary replacement error = %v", err)
			}
			canonical, readErr := os.ReadFile(filepath.Join(state, "value"))
			if noReplace {
				if !errors.Is(readErr, os.ErrNotExist) {
					t.Fatalf("create canonical = %q, %v", canonical, readErr)
				}
			} else if readErr != nil || string(canonical) != "old" {
				t.Fatalf("write canonical = %q, %v", canonical, readErr)
			}
			entries, err := opened.(SecureDirectoryReader).ReadDir()
			if err != nil {
				t.Fatal(err)
			}
			preserved := false
			for _, entry := range entries {
				if entry.Name() == "value" {
					continue
				}
				data, err := os.ReadFile(filepath.Join(state, entry.Name()))
				if err == nil && string(data) == "replacement" {
					preserved = true
				}
			}
			if !preserved {
				t.Fatalf("replacement temporary not preserved: %#v", entries)
			}
		})
	}
}

type windowsReplacingTemporaryDirectory struct {
	SecureDirectory
}

func (directory *windowsReplacingTemporaryDirectory) RenameChecked(oldName, newName string, expected SecureFileIdentity) error {
	return directory.replaceThenRename(oldName, newName, expected, false)
}

func (directory *windowsReplacingTemporaryDirectory) RenameNoReplaceChecked(oldName, newName string, expected SecureFileIdentity) error {
	return directory.replaceThenRename(oldName, newName, expected, true)
}

func (directory *windowsReplacingTemporaryDirectory) RemoveChecked(name string, expected SecureFileIdentity) error {
	return directory.SecureDirectory.(IdentityBoundRemover).RemoveChecked(name, expected)
}

func (directory *windowsReplacingTemporaryDirectory) replaceThenRename(oldName, newName string, expected SecureFileIdentity, noReplace bool) error {
	if err := directory.SecureDirectory.(IdentityBoundRemover).RemoveChecked(oldName, expected); err != nil {
		return err
	}
	replacement, err := directory.SecureDirectory.CreateExclusive(oldName, 0o600)
	if err != nil {
		return err
	}
	if _, err := replacement.Write([]byte("replacement")); err != nil {
		_ = replacement.Close()
		return err
	}
	if err := replacement.Sync(); err != nil {
		_ = replacement.Close()
		return err
	}
	if err := replacement.Close(); err != nil {
		return err
	}
	renamer := directory.SecureDirectory.(IdentityBoundRenamer)
	if noReplace {
		return renamer.RenameNoReplaceChecked(oldName, newName, expected)
	}
	return renamer.RenameChecked(oldName, newName, expected)
}

func TestWindowsSecureEntryNameRejectsWindowsAliases(t *testing.T) {
	invalid := []string{
		"", ".", "..",
		"sub/name", `sub\name`, "value:stream",
		"trailing.", "trailing ", "control\x01",
		"CON", "con.txt", "PRN", "AUX", "NUL",
		"COM1", "com9.log", "LPT1", "lpt9.txt",
		`\\?\device`, `\\.\device`,
	}
	for _, name := range invalid {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			if err := validateWindowsSecureEntryName(name); !errors.Is(err, ErrUnsafeSecurePath) {
				t.Fatalf("validateWindowsSecureEntryName(%q) error = %v", name, err)
			}
		})
	}
	if err := validateWindowsSecureEntryName("valid-name.json"); err != nil {
		t.Fatalf("valid entry error = %v", err)
	}
}

func TestWindowsRetainedReadDirectoryDoesNotExposeMutation(t *testing.T) {
	root := t.TempDir()
	nestedPath := filepath.Join(root, "provider")
	if err := os.Mkdir(nestedPath, 0o755); err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(nestedPath, "credential.json")
	if err := os.WriteFile(filePath, []byte("external"), 0o600); err != nil {
		t.Fatal(err)
	}
	fsys := newWindowsTestFileSystem(t, root)
	directory, err := fsys.OpenRetainedReadDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	assertWindowsReadOnlyDirectoryType(t, directory)
	nested, err := directory.OpenDirectory("provider")
	if err != nil {
		t.Fatal(err)
	}
	defer nested.Close()
	assertWindowsReadOnlyDirectoryType(t, nested)
	file, err := nested.OpenNoFollow("credential.json")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	data := make([]byte, len("external"))
	if _, err := file.Read(data); err != nil {
		t.Fatal(err)
	}
	if string(data) != "external" {
		t.Fatalf("retained external content = %q", data)
	}
}

func TestWindowsRetainedDirectoriesResetEnumerationAndCloseOnce(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	fys := newWindowsTestFileSystem(t, root)
	if err := EnsureSecureDirectory(fys, state); err != nil {
		t.Fatal(err)
	}
	secure, err := fys.OpenSecureDirectory(state)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "second"} {
		file, err := secure.CreateExclusive(name, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	assertWindowsDirectoryEnumerationResets(t, secure)
	if err := secure.Close(); err != nil {
		t.Fatal(err)
	}
	if err := secure.Close(); err != nil {
		t.Fatal(err)
	}
	retained, err := fys.OpenRetainedReadDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	assertWindowsDirectoryEnumerationResets(t, retained)
	if err := retained.Close(); err != nil {
		t.Fatal(err)
	}
	if err := retained.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertWindowsDirectoryEnumerationResets(t *testing.T, directory any) {
	t.Helper()
	reader, ok := directory.(SecureDirectoryReader)
	if !ok {
		t.Fatal("retained directory does not expose read-only enumeration")
	}
	readNames := func() []string {
		entries, err := reader.ReadDir()
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		return names
	}
	first := readNames()
	second := readNames()
	if !reflect.DeepEqual(first, second) || len(first) == 0 {
		t.Fatalf("directory enumerations = %v then %v", first, second)
	}
}

func assertWindowsReadOnlyDirectoryType(t *testing.T, directory RetainedReadDirectory) {
	t.Helper()
	for name, exposed := range map[string]bool{
		"SecureDirectory":        typeImplements[SecureDirectory](directory),
		"DurableDirectory":       typeImplements[DurableDirectory](directory),
		"IdentityBoundRenamer":   typeImplements[IdentityBoundRenamer](directory),
		"IdentityBoundRemover":   typeImplements[IdentityBoundRemover](directory),
		"ExclusiveLocker":        typeImplements[ExclusiveLocker](directory),
		"ExclusiveFileCreator":   typeImplements[ExclusiveFileCreator](directory),
		"DurableDirectoryOpener": typeImplements[DurableDirectoryOpener](directory),
	} {
		if exposed {
			t.Fatalf("read-only directory exposes %s", name)
		}
	}
}

func typeImplements[Interface any](value any) bool {
	_, ok := value.(Interface)
	return ok
}

func TestWindowsRetainedRegularFileReadOnlyDoesNotExposeMutation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "provider.json")
	if err := os.WriteFile(path, []byte("external"), 0o600); err != nil {
		t.Fatal(err)
	}
	fsys := newWindowsTestFileSystem(t, root)
	file, err := fsys.OpenRetainedRegularFileNoFollow(path, RetainedRegularFileReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	assertWindowsRetainedRegularFileType(t, file)
	data := make([]byte, len("external"))
	if _, err := file.Read(data); err != nil {
		t.Fatal(err)
	}
	if string(data) != "external" {
		t.Fatalf("retained regular content = %q", data)
	}
	for _, policy := range []RetainedRegularFilePolicy{0, 99} {
		if _, err := fsys.OpenRetainedRegularFileNoFollow(filepath.Join(root, "missing"), policy); err == nil || errors.Is(err, os.ErrNotExist) {
			t.Fatalf("invalid policy %d error = %v", policy, err)
		}
	}
}

func assertWindowsRetainedRegularFileType(t *testing.T, file RetainedRegularFile) {
	t.Helper()
	for name, exposed := range map[string]bool{
		"io.Writer":               typeImplements[io.Writer](file),
		"DurableFile":             typeImplements[DurableFile](file),
		"SecureDirectory":         typeImplements[SecureDirectory](file),
		"ExclusiveLocker":         typeImplements[ExclusiveLocker](file),
		"NewExclusiveLocker":      typeImplements[NewExclusiveLocker](file),
		"ExclusiveLockHeldProber": typeImplements[ExclusiveLockHeldProber](file),
		"raw handle":              typeImplements[interface{ Fd() uintptr }](file),
	} {
		if exposed {
			t.Fatalf("retained regular file exposes %s", name)
		}
	}
}

func TestWindowsRetainedExecutableDeniesReplacementUntilWait(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(bin, "helper.exe")
	user, err := currentWindowsUserSID()
	if err != nil {
		t.Fatal(err)
	}
	setWindowsTestSecurity(t, bin, fmt.Sprintf("O:%sD:P(A;;FA;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)", user.String(), user.String()))
	copyWindowsTestExecutable(t, executable)
	setWindowsTestSecurity(t, executable, fmt.Sprintf("O:%sD:P(A;;FA;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)", user.String(), user.String()))
	fys := newWindowsTestFileSystem(t, root)
	file, err := fys.OpenRetainedRegularFileNoFollow(executable, RetainedRegularFileExecutableDenyReplacement)
	if err != nil {
		t.Fatal(err)
	}
	assertWindowsRetainedRegularFileType(t, file)
	beforeInfo, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	beforeIdentity, ok := fys.FileIdentity(beforeInfo)
	if !ok {
		t.Fatal("retained executable has no complete identity")
	}
	command := exec.Command(executable, "-test.run=^TestWindowsRetainedExecutableHelperProcess$")
	command.Env = append(os.Environ(), "CQ_WINDOWS_RETAINED_HELPER=1")
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	renamedExecutable := filepath.Join(bin, "renamed.exe")
	renamedBin := filepath.Join(root, "bin-renamed")
	if writable, err := os.OpenFile(executable, os.O_WRONLY, 0); err == nil {
		_ = writable.Close()
		t.Fatal("write-capable open succeeded while executable retained")
	}
	if err := os.Rename(executable, renamedExecutable); err == nil {
		t.Fatal("executable rename succeeded while retained")
	}
	if err := os.Remove(executable); err == nil {
		t.Fatal("executable removal succeeded while retained")
	}
	if err := os.Rename(bin, renamedBin); err == nil {
		t.Fatal("ancestor rename succeeded while executable retained")
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	afterInfo, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	afterIdentity, ok := fys.FileIdentity(afterInfo)
	if !ok || !SameSecureObject(beforeIdentity, afterIdentity) {
		t.Fatalf("retained executable identity changed: %#v -> %#v", beforeIdentity, afterIdentity)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(executable, renamedExecutable); err != nil {
		t.Fatalf("rename after close: %v", err)
	}
	if err := os.Rename(bin, renamedBin); err != nil {
		t.Fatalf("ancestor rename after close: %v", err)
	}
}

func TestWindowsRetainedRegularFileSerialisesReadStatAndClose(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "provider.json")
	if err := os.WriteFile(path, []byte("external"), 0o600); err != nil {
		t.Fatal(err)
	}
	fys := newWindowsTestFileSystem(t, root)
	file, err := fys.OpenRetainedRegularFileNoFollow(path, RetainedRegularFileReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errorsByOperation := make(chan error, 3)
	var wait sync.WaitGroup
	wait.Add(3)
	go func() {
		defer wait.Done()
		<-start
		_, err := file.Read(make([]byte, 1))
		if err != nil && !errors.Is(err, os.ErrClosed) && !errors.Is(err, io.EOF) {
			errorsByOperation <- err
		}
	}()
	go func() {
		defer wait.Done()
		<-start
		_, err := file.Stat()
		if err != nil && !errors.Is(err, os.ErrClosed) {
			errorsByOperation <- err
		}
	}()
	go func() {
		defer wait.Done()
		<-start
		if err := file.Close(); err != nil {
			errorsByOperation <- err
		}
	}()
	close(start)
	wait.Wait()
	close(errorsByOperation)
	for err := range errorsByOperation {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("post-close read error = %v", err)
	}
	if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("post-close stat error = %v", err)
	}
}

func TestWindowsRetainedRegularFileClosesEveryHandleOnce(t *testing.T) {
	root := t.TempDir()
	open := func(name string) *os.File {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		return file
	}
	boundary := open("boundary")
	leaf := open("leaf")
	final := open("final")
	errorsByName := map[string]error{
		"boundary": errors.New("boundary close"),
		"leaf":     errors.New("leaf close"),
		"final":    errors.New("final close"),
	}
	var order []string
	file := &windowsRetainedRegularFile{
		file:      final,
		ancestors: []*os.File{boundary, leaf},
		closeFile: func(handle *os.File) error {
			name := filepath.Base(handle.Name())
			order = append(order, name)
			return errors.Join(handle.Close(), errorsByName[name])
		},
	}
	first := file.Close()
	second := file.Close()
	if !reflect.DeepEqual(order, []string{"final", "leaf", "boundary"}) {
		t.Fatalf("close order = %v", order)
	}
	for name, expected := range errorsByName {
		if !errors.Is(first, expected) || !errors.Is(second, expected) {
			t.Fatalf("%s close error missing: first=%v second=%v", name, first, second)
		}
	}
}

func TestWindowsRetainedExecutableHelperProcess(t *testing.T) {
	if os.Getenv("CQ_WINDOWS_RETAINED_HELPER") != "1" {
		return
	}
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestWindowsRetainedExecutableUsesFixedRightsAndShareReadOnly(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(bin, "helper.exe")
	if err := os.WriteFile(executable, []byte("helper"), 0o600); err != nil {
		t.Fatal(err)
	}
	user, err := currentWindowsUserSID()
	if err != nil {
		t.Fatal(err)
	}
	strict := fmt.Sprintf("O:%sD:P(A;;FA;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)", user.String(), user.String())
	setWindowsTestSecurity(t, bin, strict)
	setWindowsTestSecurity(t, executable, strict)
	fys := newWindowsTestFileSystem(t, root)
	selection, err := fys.resolveSecureBoundary(executable, secureBoundaryExternalFile)
	if err != nil {
		t.Fatal(err)
	}
	boundary, err := fys.windowsPathBoundary(selection, secureBoundaryExternalFile)
	if err != nil {
		t.Fatal(err)
	}
	type request struct {
		name                       string
		access, share, disposition uint32
		options                    uint32
	}
	var requests []request
	file, err := openWindowsRetainedExecutableWith(executable, boundary, func(parent windows.Handle, name string, access, share, disposition, options uint32, descriptor *windows.SECURITY_DESCRIPTOR) (windowsOpenResult, error) {
		requests = append(requests, request{name: name, access: access, share: share, disposition: disposition, options: options})
		return openWindowsRelative(parent, name, access, share, disposition, options, descriptor)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	anchorIndex := -1
	for index, request := range requests {
		if strings.EqualFold(request.name, filepath.Base(root)) {
			anchorIndex = index
		}
	}
	if anchorIndex < 0 || len(requests)-anchorIndex != 3 {
		t.Fatalf("retained requests = %#v", requests)
	}
	directoryAccess := uint32(windows.FILE_LIST_DIRECTORY | windows.FILE_TRAVERSE | windows.FILE_READ_ATTRIBUTES | windows.READ_CONTROL | windows.SYNCHRONIZE)
	directoryShare := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE)
	effectiveOptions := uint32(windows.FILE_OPEN_REPARSE_POINT | windows.FILE_SYNCHRONOUS_IO_NONALERT)
	for _, request := range requests[anchorIndex : len(requests)-1] {
		if request.access != directoryAccess || request.share != directoryShare || request.disposition != windows.FILE_OPEN || request.options|effectiveOptions != windows.FILE_DIRECTORY_FILE|effectiveOptions {
			t.Fatalf("directory request = %#v", request)
		}
	}
	final := requests[len(requests)-1]
	finalAccess := uint32(windows.FILE_READ_DATA | windows.FILE_EXECUTE | windows.FILE_READ_ATTRIBUTES | windows.READ_CONTROL | windows.SYNCHRONIZE)
	if final.access != finalAccess || final.share != windows.FILE_SHARE_READ || final.disposition != windows.FILE_OPEN || final.options|effectiveOptions != windows.FILE_NON_DIRECTORY_FILE|effectiveOptions {
		t.Fatalf("final request = %#v", final)
	}
}

func TestWindowsRetainedExecutableRejectsReparseAndPreexistingWriter(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "helper.exe")
	if err := os.WriteFile(executable, []byte("helper"), 0o600); err != nil {
		t.Fatal(err)
	}
	user, err := currentWindowsUserSID()
	if err != nil {
		t.Fatal(err)
	}
	strict := fmt.Sprintf("O:%sD:P(A;;FA;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)", user.String(), user.String())
	setWindowsTestSecurity(t, executable, strict)
	fys := newWindowsTestFileSystem(t, root)
	writer, err := os.OpenFile(executable, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if file, err := fys.OpenRetainedRegularFileNoFollow(executable, RetainedRegularFileExecutableDenyReplacement); err == nil {
		_ = file.Close()
		t.Fatal("retained executable open accepted pre-existing writer")
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "helper-link.exe")
	if err := os.Symlink(executable, link); err != nil {
		t.Fatal(err)
	}
	if file, err := fys.OpenRetainedRegularFileNoFollow(link, RetainedRegularFileExecutableDenyReplacement); !errors.Is(err, ErrUnsafeSecurePath) {
		if file != nil {
			_ = file.Close()
		}
		t.Fatalf("retained reparse error = %v", err)
	}
}

func copyWindowsTestExecutable(t *testing.T, destination string) {
	t.Helper()
	sourcePath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	target, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(target, source); err != nil {
		_ = target.Close()
		t.Fatal(err)
	}
	if err := target.Sync(); err != nil {
		_ = target.Close()
		t.Fatal(err)
	}
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsSecureMetadataAcceptsExactPrivateDACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "value")
	if err := os.WriteFile(path, []byte("trusted"), 0o600); err != nil {
		t.Fatal(err)
	}
	user, err := currentWindowsUserSID()
	if err != nil {
		t.Fatal(err)
	}
	setWindowsTestSecurity(t, path, fmt.Sprintf("O:%sD:P(A;;FA;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)", user.String(), user.String()))
	fsys := newWindowsTestFileSystem(t, filepath.Dir(path))
	if err := ValidateSecureRegularFile(fsys, path); err != nil {
		t.Fatal(err)
	}
	info, err := fsys.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	identity, ok := fsys.FileIdentity(info)
	if !ok || identity.FileID == ([16]byte{}) || identity.Links != 1 {
		t.Fatalf("identity = %#v, %v", identity, ok)
	}
}

func TestWindowsSecureMetadataRejectsBroadDACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "value")
	if err := os.WriteFile(path, []byte("unsafe"), 0o600); err != nil {
		t.Fatal(err)
	}
	setWindowsTestDACL(t, path, "D:P(A;;FA;;;WD)", true)
	fsys := newWindowsTestFileSystem(t, filepath.Dir(path))
	if err := ValidateSecureRegularFile(fsys, path); !errors.Is(err, ErrUnsafeSecurePath) {
		t.Fatalf("error = %v, want ErrUnsafeSecurePath", err)
	}
}

func TestWindowsSecureMetadataRejectsReparsePoint(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	fsys := newWindowsTestFileSystem(t, dir)
	if _, err := fsys.Lstat(link); !errors.Is(err, ErrUnsafeSecurePath) {
		t.Fatalf("error = %v, want ErrUnsafeSecurePath", err)
	}
}

func TestWindowsSecureMetadataRejectsDirectoryReparsePoint(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	fsys := newWindowsTestFileSystem(t, dir)
	if _, err := fsys.Lstat(link); !errors.Is(err, ErrUnsafeSecurePath) {
		t.Fatalf("error = %v, want ErrUnsafeSecurePath", err)
	}
}

func TestWindowsSecureMetadataRejectsHardLink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "value")
	link := filepath.Join(dir, "other")
	if err := os.WriteFile(path, []byte("trusted"), 0o600); err != nil {
		t.Fatal(err)
	}
	user, err := currentWindowsUserSID()
	if err != nil {
		t.Fatal(err)
	}
	setWindowsTestSecurity(t, path, fmt.Sprintf("O:%sD:P(A;;FA;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)", user.String(), user.String()))
	if err := os.Link(path, link); err != nil {
		t.Fatal(err)
	}
	fsys := newWindowsTestFileSystem(t, dir)
	info, err := fsys.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	identity, ok := fsys.FileIdentity(info)
	if !ok || identity.Links != 2 {
		t.Fatalf("identity = %#v, %v", identity, ok)
	}
	if err := ValidateSecureRegularFile(fsys, path); !errors.Is(err, ErrUnsafeSecurePath) {
		t.Fatalf("error = %v, want ErrUnsafeSecurePath", err)
	}
}

func TestWindowsSecureMetadataRejectsNonExactPrivateDACLs(t *testing.T) {
	user, err := currentWindowsUserSID()
	if err != nil {
		t.Fatal(err)
	}
	userText := user.String()
	tests := map[string]struct {
		dacl      string
		protected bool
	}{
		"unprotected":            {fmt.Sprintf("D:(A;;FA;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)", userText), false},
		"missing administrators": {fmt.Sprintf("D:P(A;;FA;;;%s)(A;;FA;;;SY)", userText), true},
		"duplicate system":       {fmt.Sprintf("D:P(A;;FA;;;%s)(A;;FA;;;SY)(A;;FA;;;SY)(A;;FA;;;BA)", userText), true},
		"everyone read":          {fmt.Sprintf("D:P(A;;FA;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)(A;;GR;;;WD)", userText), true},
		"authenticated full":     {fmt.Sprintf("D:P(A;;FA;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;AU)", userText), true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "value")
			if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
				t.Fatal(err)
			}
			setWindowsTestDACL(t, path, test.dacl, test.protected)
			fsys := newWindowsTestFileSystem(t, dir)
			if err := ValidateSecureRegularFile(fsys, path); !errors.Is(err, ErrUnsafeSecurePath) {
				t.Fatalf("error = %v, want ErrUnsafeSecurePath", err)
			}
		})
	}
}

func TestWindowsRetainedAncestorAcceptsTrustedACLWithInheritedRead(t *testing.T) {
	anchor := t.TempDir()
	user, err := currentWindowsUserSID()
	if err != nil {
		t.Fatal(err)
	}
	setWindowsTestSecurity(t, anchor, fmt.Sprintf("O:%sD:AI(A;OICI;FA;;;%s)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICIIOID;GRGX;;;WD)", user.String(), user.String()))
	fsys := newWindowsTestFileSystem(t, anchor)
	info, err := fsys.Lstat(anchor)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsys.ValidateRetainedAncestor(info); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSecureDirectory(fsys, anchor); !errors.Is(err, ErrUnsafeSecurePath) {
		t.Fatalf("error = %v, want ErrUnsafeSecurePath", err)
	}
}

func TestWindowsRetainedAncestorRejectsUntrustedGenericWrite(t *testing.T) {
	testWindowsRetainedAncestorRejected(t, "WD", "GW")
}

func TestWindowsRetainedAncestorRejectsUntrustedDeleteChild(t *testing.T) {
	testWindowsRetainedAncestorRejected(t, "AU", "DC")
}

func TestWindowsBoundaryIdentityRejectsReplacement(t *testing.T) {
	parent := t.TempDir()
	anchor := filepath.Join(parent, "anchor")
	replacement := filepath.Join(parent, "replacement")
	displaced := filepath.Join(parent, "displaced")
	for _, path := range []string{anchor, replacement} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fsys := newWindowsTestFileSystem(t, anchor)
	if err := os.Rename(anchor, displaced); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, anchor); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.Lstat(anchor); !errors.Is(err, ErrUnsafeSecurePath) {
		t.Fatalf("error = %v, want ErrUnsafeSecurePath", err)
	}
}

func TestWindowsBoundaryResolversRemainValueScoped(t *testing.T) {
	parent := t.TempDir()
	first := filepath.Join(parent, "first")
	second := filepath.Join(parent, "second")
	for _, path := range []string{first, second} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	filesystems := []struct {
		fsys OSFileSystem
		own  string
		peer string
	}{
		{fsys: newWindowsTestFileSystem(t, first), own: first, peer: second},
		{fsys: newWindowsTestFileSystem(t, second), own: second, peer: first},
	}
	errorsByValue := make(chan error, len(filesystems))
	for _, filesystem := range filesystems {
		filesystem := filesystem
		go func() {
			if _, err := filesystem.fsys.Lstat(filesystem.own); err != nil {
				errorsByValue <- err
				return
			}
			if _, err := filesystem.fsys.Lstat(filesystem.peer); !errors.Is(err, ErrUnsafeSecurePath) {
				errorsByValue <- fmt.Errorf("cross-boundary error = %v, want ErrUnsafeSecurePath", err)
				return
			}
			errorsByValue <- nil
		}()
	}
	for range filesystems {
		if err := <-errorsByValue; err != nil {
			t.Fatal(err)
		}
	}
}

func TestWindowsZeroValueBoundaryIgnoresEnvironmentAndGlobalState(t *testing.T) {
	anchors, err := userdirs.WindowsAppDataAnchors()
	if err != nil {
		t.Fatal(err)
	}
	attacker := t.TempDir()
	for _, name := range []string{"APPDATA", "LOCALAPPDATA", "USERPROFILE"} {
		t.Setenv(name, attacker)
	}
	fsys := OSFileSystem{}
	target := filepath.Join(anchors.LocalAppData, "cq", "nonce")
	selection, err := fsys.resolveSecureBoundary(target, secureBoundaryCQPrivate)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(selection.AnchorPath, anchors.LocalAppData) || strings.HasPrefix(strings.ToLower(selection.AnchorPath), strings.ToLower(attacker)) {
		t.Fatalf("selection = %#v, anchors = %#v", selection, anchors)
	}
	boundary, err := fsys.windowsPathBoundary(selection, secureBoundaryCQPrivate)
	if err != nil {
		t.Fatal(err)
	}
	anchorInfo, err := fsys.Lstat(anchors.LocalAppData)
	if err != nil {
		t.Fatal(err)
	}
	wantIdentity, ok := fsys.FileIdentity(anchorInfo)
	if !ok || !SameSecureObject(boundary.AnchorIdentity, wantIdentity) {
		t.Fatalf("boundary identity = %#v, want %#v", boundary.AnchorIdentity, wantIdentity)
	}
	if _, err := fsys.resolveSecureBoundary(filepath.Join(attacker, "cq"), secureBoundaryCQPrivate); !errors.Is(err, ErrUnsafeSecurePath) {
		t.Fatalf("temporary boundary error = %v, want ErrUnsafeSecurePath", err)
	}
}

func TestWindowsSecureOpenRejectsBroadIntermediateBelowAnchor(t *testing.T) {
	anchor := t.TempDir()
	intermediate := filepath.Join(anchor, "intermediate")
	target := filepath.Join(intermediate, "target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	user, err := currentWindowsUserSID()
	if err != nil {
		t.Fatal(err)
	}
	setWindowsTestSecurity(t, intermediate, fmt.Sprintf("O:%sD:P(A;;FA;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)(A;;GRGX;;;WD)", user.String(), user.String()))
	setWindowsTestSecurity(t, target, fmt.Sprintf("O:%sD:P(A;;FA;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)", user.String(), user.String()))
	fsys := newWindowsTestFileSystem(t, anchor)
	selection, err := fsys.resolveSecureBoundary(target, secureBoundaryCQPrivate)
	if err != nil {
		t.Fatal(err)
	}
	boundary, err := fsys.windowsPathBoundary(selection, secureBoundaryCQPrivate)
	if err != nil {
		t.Fatal(err)
	}
	file, err := openWindowsAbsolutePath(target, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.SYNCHRONIZE, windows.FILE_DIRECTORY_FILE, boundary)
	if file != nil {
		_ = file.Close()
	}
	if !errors.Is(err, ErrUnsafeSecurePath) {
		t.Fatalf("error = %v, want ErrUnsafeSecurePath", err)
	}
}

func TestWindowsOSFileSystemHasNoExportedBoundaryField(t *testing.T) {
	typeOf := reflect.TypeOf(OSFileSystem{})
	for index := 0; index < typeOf.NumField(); index++ {
		field := typeOf.Field(index)
		if field.IsExported() {
			t.Fatalf("exported OSFileSystem field %q", field.Name)
		}
	}
}

func TestWindowsAbsolutePathValidationRejectsUnsupportedNamespaces(t *testing.T) {
	tests := map[string]string{
		"empty":            "",
		"relative":         `relative\path`,
		"drive relative":   `C:relative`,
		"UNC":              `\\server\share\value`,
		"extended":         `\\?\C:\value`,
		"device":           `\\.\C:\value`,
		"alternate stream": `C:\value:stream`,
		"non-clean":        `C:\value\..\other`,
	}
	for name, path := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := validateWindowsAbsolutePath(path); !errors.Is(err, ErrUnsafeSecurePath) {
				t.Fatalf("error = %v, want ErrUnsafeSecurePath", err)
			}
		})
	}
}

func TestWindowsExternalCredentialDirectoryAcceptsInheritedReadTraverse(t *testing.T) {
	root := t.TempDir()
	user, err := currentWindowsUserSID()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".codex", "accounts"} {
		path := filepath.Join(root, name)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		setWindowsTestSecurity(t, path, fmt.Sprintf("O:%sD:AI(A;OICI;FA;;;%s)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICIIOID;GRGX;;;WD)", user.String(), user.String()))
		fsys := newWindowsTestFileSystem(t, path)
		if err := ValidateExternalCredentialDirectory(fsys, path); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := ValidateSecureDirectory(fsys, path); !errors.Is(err, ErrUnsafeSecurePath) {
			t.Fatalf("%s private error = %v, want ErrUnsafeSecurePath", name, err)
		}
	}
}

func TestWindowsExternalCredentialDirectoryRejectsUntrustedMutation(t *testing.T) {
	user, err := currentWindowsUserSID()
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]uint32{
		"add file":         windowsFileAddFile,
		"add subdirectory": windowsFileAddSubdirectory,
		"delete child":     windowsFileDeleteChild,
		"write EA":         windows.FILE_WRITE_EA,
		"write attributes": windows.FILE_WRITE_ATTRIBUTES,
		"delete":           windows.DELETE,
		"write DACL":       windows.WRITE_DAC,
		"write owner":      windows.WRITE_OWNER,
		"generic write":    windows.GENERIC_WRITE,
		"generic all":      windows.GENERIC_ALL,
	}
	for name, rights := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "accounts")
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			setWindowsTestSecurity(t, path, fmt.Sprintf("O:%sD:P(A;;FA;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x%08x;;;WD)", user.String(), user.String(), rights))
			fsys := newWindowsTestFileSystem(t, path)
			if err := ValidateExternalCredentialDirectory(fsys, path); !errors.Is(err, ErrUnsafeSecurePath) {
				t.Fatalf("error = %v, want ErrUnsafeSecurePath", err)
			}
		})
	}
}

func testWindowsRetainedAncestorRejected(t *testing.T, principal, rights string) {
	t.Helper()
	anchor := t.TempDir()
	user, err := currentWindowsUserSID()
	if err != nil {
		t.Fatal(err)
	}
	setWindowsTestSecurity(t, anchor, fmt.Sprintf("O:%sD:P(A;;FA;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)(A;;%s;;;%s)", user.String(), user.String(), rights, principal))
	fsys := newWindowsTestFileSystem(t, anchor)
	info, err := fsys.Lstat(anchor)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsys.ValidateRetainedAncestor(info); !errors.Is(err, ErrUnsafeSecurePath) {
		t.Fatalf("error = %v, want ErrUnsafeSecurePath", err)
	}
}

func TestWindowsSecurityClassificationUsesNamedPolicies(t *testing.T) {
	user, err := currentWindowsUserSID()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		sddl       string
		want       windowsSecurityClassification
		wantErr    bool
		checkOwner bool
	}{
		{
			name: "exact private",
			sddl: fmt.Sprintf("O:%sD:P(A;;FA;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)", user.String(), user.String()),
			want: windowsSecurityClassification{
				PrivateDACL: true, AncestorSafe: true, ExternalCredentialDirectorySafe: true,
				ExternalCredentialSafe: true, ExternalCacheSafe: true, ExternalImportFileSafe: true,
			},
			checkOwner: true,
		},
		{
			name: "untrusted read",
			sddl: fmt.Sprintf("O:%sD:P(A;;FA;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)(A;;GRGX;;;WD)", user.String(), user.String()),
			want: windowsSecurityClassification{
				AncestorSafe: true, ExternalCredentialDirectorySafe: true,
				ExternalCacheSafe: true, ExternalImportFileSafe: true,
			},
			checkOwner: true,
		},
		{
			name:       "untrusted mutation",
			sddl:       fmt.Sprintf("O:%sD:P(A;;FA;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)(A;;GW;;;WD)", user.String(), user.String()),
			want:       windowsSecurityClassification{},
			checkOwner: true,
		},
		{
			name: "trusted non-current owner",
			sddl: fmt.Sprintf("O:BAD:P(A;;FA;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)", user.String()),
			want: windowsSecurityClassification{
				AncestorSafe: true, ExternalCredentialDirectorySafe: true,
				ExternalImportFileSafe: true,
			},
		},
		{
			name:    "deny ACE",
			sddl:    fmt.Sprintf("O:%sD:P(D;;GR;;;WD)(A;;FA;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)", user.String(), user.String()),
			wantErr: true,
		},
	}
	currentPrincipal, ok := windowsPrincipal(user)
	if !ok {
		t.Fatal("current principal unavailable")
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			descriptor, err := windows.SecurityDescriptorFromString(test.sddl)
			if err != nil {
				t.Fatal(err)
			}
			got, err := classifyWindowsSecurityDescriptor(descriptor, user)
			if test.wantErr {
				if !errors.Is(err, ErrUnsafeSecurePath) {
					t.Fatalf("error = %v, want ErrUnsafeSecurePath", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.checkOwner && got.Owner != currentPrincipal {
				t.Fatalf("owner = %#v, want %#v", got.Owner, currentPrincipal)
			}
			got.Owner = SecurePrincipal{}
			if got != test.want {
				t.Fatalf("classification = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestWindowsSecurityClassificationRejectsNullDACL(t *testing.T) {
	user, err := currentWindowsUserSID()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.NewSecurityDescriptor()
	if err != nil {
		t.Fatal(err)
	}
	if err := descriptor.SetOwner(user, false); err != nil {
		t.Fatal(err)
	}
	if err := descriptor.SetDACL(nil, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := classifyWindowsSecurityDescriptor(descriptor, user); !errors.Is(err, ErrUnsafeSecurePath) {
		t.Fatalf("error = %v, want ErrUnsafeSecurePath", err)
	}
}

func TestWindowsACLParserAllowsTrailingCapacity(t *testing.T) {
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;SY)")
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	header := unsafe.Slice((*byte)(unsafe.Pointer(acl)), 8)
	size := int(binary.LittleEndian.Uint16(header[2:4]))
	buffer := make([]byte, size+8)
	copy(buffer, unsafe.Slice((*byte)(unsafe.Pointer(acl)), size))
	binary.LittleEndian.PutUint16(buffer[2:4], uint16(len(buffer)))
	if _, err := parseWindowsACL((*windows.ACL)(unsafe.Pointer(&buffer[0]))); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsACLParserRejectsMalformedMemory(t *testing.T) {
	tests := map[string][]byte{
		"short ACL":       {2: 7},
		"truncated ACE":   {2: 8, 4: 1},
		"short ACE":       {2: 24, 4: 1, 8: windows.ACCESS_ALLOWED_ACE_TYPE, 10: 12},
		"unsupported ACE": {2: 24, 4: 1, 8: windows.ACCESS_DENIED_ACE_TYPE, 10: 16, 16: 1},
		"truncated SID":   {2: 24, 4: 1, 8: windows.ACCESS_ALLOWED_ACE_TYPE, 10: 16, 16: 1, 17: 15},
	}
	for name, header := range tests {
		t.Run(name, func(t *testing.T) {
			buffer := make([]byte, 24)
			copy(buffer, header)
			if _, err := parseWindowsACL((*windows.ACL)(unsafe.Pointer(&buffer[0]))); !errors.Is(err, ErrUnsafeSecurePath) {
				t.Fatalf("error = %v, want ErrUnsafeSecurePath", err)
			}
		})
	}
}

func TestWindowsRemoteProtocolInfoLayout(t *testing.T) {
	var info windowsFileRemoteProtocolInfo
	checks := map[string]struct {
		got  uintptr
		want uintptr
	}{
		"size":                       {unsafe.Sizeof(info), 180},
		"structure version":          {unsafe.Offsetof(info.StructureVersion), 0},
		"structure size":             {unsafe.Offsetof(info.StructureSize), 2},
		"protocol":                   {unsafe.Offsetof(info.Protocol), 4},
		"protocol major":             {unsafe.Offsetof(info.ProtocolMajorVersion), 8},
		"protocol minor":             {unsafe.Offsetof(info.ProtocolMinorVersion), 10},
		"protocol revision":          {unsafe.Offsetof(info.ProtocolRevision), 12},
		"reserved":                   {unsafe.Offsetof(info.Reserved), 14},
		"flags":                      {unsafe.Offsetof(info.Flags), 16},
		"generic reserved":           {unsafe.Offsetof(info.GenericReserved), 20},
		"protocol-specific reserved": {unsafe.Offsetof(info.ProtocolSpecificReserved), 52},
		"protocol-specific":          {unsafe.Offsetof(info.ProtocolSpecific), 116},
	}
	for name, check := range checks {
		if check.got != check.want {
			t.Errorf("%s offset = %d, want %d", name, check.got, check.want)
		}
	}
}

func setWindowsTestSecurity(t *testing.T, path, sddl string) {
	t.Helper()
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := descriptor.Control()
	if err != nil {
		t.Fatal(err)
	}
	information := windows.SECURITY_INFORMATION(windows.OWNER_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION)
	protected := control&windows.SE_DACL_PROTECTED != 0
	if protected {
		information |= windows.PROTECTED_DACL_SECURITY_INFORMATION
	} else {
		information |= windows.UNPROTECTED_DACL_SECURITY_INFORMATION
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, information, owner, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	assertWindowsTestDACLProtection(t, path, protected)
}

func setWindowsTestDACL(t *testing.T, path, sddl string, protected bool) {
	t.Helper()
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	information := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION)
	if protected {
		information |= windows.PROTECTED_DACL_SECURITY_INFORMATION
	} else {
		information |= windows.UNPROTECTED_DACL_SECURITY_INFORMATION
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, information, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	assertWindowsTestDACLProtection(t, path, protected)
}

func assertWindowsTestDACLProtection(t *testing.T, path string, wantProtected bool) {
	t.Helper()
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	flags := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.IsDir() {
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	handle, err := windows.CreateFile(pointer, windows.READ_CONTROL, windowsShareAll, nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := descriptor.Control()
	if err != nil {
		t.Fatal(err)
	}
	if got := control&windows.SE_DACL_PROTECTED != 0; got != wantProtected {
		t.Fatalf("DACL protected = %v, want %v", got, wantProtected)
	}
}
