//go:build windows

package fsutil

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
