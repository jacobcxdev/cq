//go:build windows

package userdirs

import (
	"encoding/binary"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func TestCurrentUserAppDataAnchorsUsesOneToken(t *testing.T) {
	const token = windows.Token(41)
	var opened, tokenClosed, shellOpened, shellClosed, resolved int
	shellFolders := &fakeWindowsUserShellFolders{}
	want := AppDataAnchors{
		RoamingAppData: `E:\Redirected\Roaming`,
		LocalAppData:   `D:\Redirected\Local`,
		UserProfile:    `C:\Users\alice`,
	}

	anchors, err := currentUserAppDataAnchorsWith(
		func() (windows.Token, func() error, error) {
			opened++
			return token, func() error { tokenClosed++; return nil }, nil
		},
		func(gotToken windows.Token) (WindowsUserShellFolders, func() error, error) {
			shellOpened++
			if gotToken != token {
				t.Fatalf("shell-folder token = %v", gotToken)
			}
			return shellFolders, func() error { shellClosed++; return nil }, nil
		},
		func(gotToken windows.Token, gotShellFolders WindowsUserShellFolders) (AppDataAnchors, error) {
			resolved++
			if gotToken != token || gotShellFolders != shellFolders {
				t.Fatalf("resolver capabilities = %v/%T", gotToken, gotShellFolders)
			}
			return want, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if anchors != want {
		t.Fatalf("anchors = %#v, want %#v", anchors, want)
	}
	if opened != 1 || tokenClosed != 1 || shellOpened != 1 || shellClosed != 1 || resolved != 1 {
		t.Fatalf("calls token/shell/resolve = %d/%d %d/%d %d", opened, tokenClosed, shellOpened, shellClosed, resolved)
	}
}

func TestCurrentUserAppDataAnchorsClosesCapabilitiesOnFailure(t *testing.T) {
	wantErr := errors.New("resolve failed")
	shellCloseErr := errors.New("shell close failed")
	tokenCloseErr := errors.New("token close failed")
	var shellClosed, tokenClosed int
	_, err := currentUserAppDataAnchorsWith(
		func() (windows.Token, func() error, error) {
			return 9, func() error { tokenClosed++; return tokenCloseErr }, nil
		},
		func(windows.Token) (WindowsUserShellFolders, func() error, error) {
			return &fakeWindowsUserShellFolders{}, func() error { shellClosed++; return shellCloseErr }, nil
		},
		func(windows.Token, WindowsUserShellFolders) (AppDataAnchors, error) { return AppDataAnchors{}, wantErr },
	)
	if !errors.Is(err, wantErr) || !errors.Is(err, shellCloseErr) || !errors.Is(err, tokenCloseErr) || shellClosed != 1 || tokenClosed != 1 {
		t.Fatalf("error/closed = %v/%d/%d", err, shellClosed, tokenClosed)
	}
}

func TestCurrentUserAppDataAnchorsClosesTokenWhenShellOpenFails(t *testing.T) {
	wantErr := errors.New("shell open failed")
	var tokenClosed, resolved int
	_, err := currentUserAppDataAnchorsWith(
		func() (windows.Token, func() error, error) {
			return 9, func() error { tokenClosed++; return nil }, nil
		},
		func(windows.Token) (WindowsUserShellFolders, func() error, error) {
			return nil, nil, wantErr
		},
		func(windows.Token, WindowsUserShellFolders) (AppDataAnchors, error) {
			resolved++
			return AppDataAnchors{}, nil
		},
	)
	if !errors.Is(err, wantErr) || tokenClosed != 1 || resolved != 0 {
		t.Fatalf("error/token close/resolve calls = %v/%d/%d", err, tokenClosed, resolved)
	}
}

func TestOpenCurrentUserShellFoldersUsesExactTokenSIDKey(t *testing.T) {
	const token = windows.Token(71)
	sid := testSID(t, "S-1-5-21-100-200-300-1001")
	wantErr := errors.New("stop after authority check")
	_, _, err := openCurrentUserShellFoldersWith(
		token,
		func(gotToken windows.Token) (*windows.SID, error) {
			if gotToken != token {
				t.Fatalf("token = %v", gotToken)
			}
			return sid, nil
		},
		func(root registry.Key, path string, access uint32) (registryValueKey, error) {
			if root != registry.USERS {
				t.Fatalf("registry root = %v", root)
			}
			wantPath := sid.String() + `\` + userShellFoldersKey
			if path != wantPath || access != registry.QUERY_VALUE {
				t.Fatalf("path/access = %q/%#x, want %q/%#x", path, access, wantPath, registry.QUERY_VALUE)
			}
			return nil, wantErr
		},
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v", err)
	}
}

func TestOpenCurrentUserShellFoldersCopiesAndClosesSubject(t *testing.T) {
	const token = windows.Token(72)
	sid := testSID(t, "S-1-5-21-100-200-300-1001")
	key := &fakeRegistryKey{}
	shellFolders, closeShellFolders, err := openCurrentUserShellFoldersWith(
		token,
		func(gotToken windows.Token) (*windows.SID, error) {
			if gotToken != token {
				t.Fatalf("token = %v", gotToken)
			}
			return sid, nil
		},
		func(root registry.Key, path string, access uint32) (registryValueKey, error) {
			if root != registry.USERS || path != sid.String()+`\`+userShellFoldersKey || access != registry.QUERY_VALUE {
				t.Fatalf("root/path/access = %v/%q/%#x", root, path, access)
			}
			return key, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	openedShellFolders, ok := shellFolders.(*registryUserShellFolders)
	if !ok || openedShellFolders.subjectSID == sid || !windows.EqualSid(openedShellFolders.subjectSID, sid) {
		t.Fatalf("stored subject SID/copy/type = %v/%t/%T", openedShellFolders, ok && openedShellFolders.subjectSID == sid, shellFolders)
	}
	gotSID, err := shellFolders.SubjectUserSID()
	if err != nil || gotSID == sid || !windows.EqualSid(gotSID, sid) {
		t.Fatalf("subject SID/copy/error = %v/%t/%v", gotSID, gotSID == sid, err)
	}
	if err := closeShellFolders(); err != nil {
		t.Fatal(err)
	}
	if key.closeCalls != 1 {
		t.Fatalf("key close calls = %d", key.closeCalls)
	}
}

func TestOpenCurrentUserShellFoldersRejectsInvalidSIDBeforeOpen(t *testing.T) {
	for _, test := range []struct {
		name string
		sid  *windows.SID
	}{
		{name: "nil", sid: nil},
		{name: "invalid", sid: new(windows.SID)},
	} {
		t.Run(test.name, func(t *testing.T) {
			openCalls := 0
			_, _, err := openCurrentUserShellFoldersWith(
				1,
				func(windows.Token) (*windows.SID, error) { return test.sid, nil },
				func(registry.Key, string, uint32) (registryValueKey, error) {
					openCalls++
					return &fakeRegistryKey{}, nil
				},
			)
			if err == nil || openCalls != 0 {
				t.Fatalf("error/open calls = %v/%d", err, openCalls)
			}
		})
	}
}

func TestRegistryUserShellFoldersReturnsSubjectSIDCopy(t *testing.T) {
	sid := testSID(t, "S-1-5-21-100-200-300-1001")
	shellFolders := &registryUserShellFolders{subjectSID: sid}
	got, err := shellFolders.SubjectUserSID()
	if err != nil || got == sid || !windows.EqualSid(got, sid) {
		t.Fatalf("subject SID/copy/error = %v/%t/%v", got, got == sid, err)
	}
}

func TestResolveWindowsAppDataForSubjectUsesMatchingSubject(t *testing.T) {
	const token = windows.Token(51)
	sid := testSID(t, "S-1-5-21-100-200-300-1001")
	shellFolders := &fakeWindowsUserShellFolders{
		subjectSID: sid,
		values: map[string]fakeRegistryValue{
			roamingAppDataValue: {raw: registryUTF16(`%USERPROFILE%\Redirected\Roaming`), valueType: registry.EXPAND_SZ},
			localAppDataValue:   {raw: registryUTF16(`D:\Redirected\Local`), valueType: registry.SZ},
		},
	}
	var profileCalls, expandCalls int
	anchors, err := resolveWindowsAppDataAnchorsWith(
		token,
		shellFolders,
		func(gotToken windows.Token) (*windows.SID, error) {
			if gotToken != token {
				t.Fatalf("token SID token = %v", gotToken)
			}
			return sid, nil
		},
		func(gotToken windows.Token, source, destination *uint16, size uint32) (bool, error) {
			expandCalls++
			if gotToken != token || windows.UTF16PtrToString(source) != `%USERPROFILE%\Redirected\Roaming` {
				t.Fatalf("expand token/source = %v/%q", gotToken, windows.UTF16PtrToString(source))
			}
			writeUTF16(t, destination, size, `E:\Redirected\Roaming`)
			return true, nil
		},
		profileProc(t, token, `C:\Users\alice`, &profileCalls),
	)
	want := AppDataAnchors{RoamingAppData: `E:\Redirected\Roaming`, LocalAppData: `D:\Redirected\Local`, UserProfile: `C:\Users\alice`}
	if err != nil || anchors != want {
		t.Fatalf("anchors/error = %#v/%v, want %#v", anchors, err, want)
	}
	if shellFolders.getValueCalls != 8 || shellFolders.subjectCalls != 1 || profileCalls != 2 || expandCalls != 1 {
		t.Fatalf("calls registry/subject/profile/expand = %d/%d/%d/%d", shellFolders.getValueCalls, shellFolders.subjectCalls, profileCalls, expandCalls)
	}
}

func TestResolveWindowsAppDataForSubjectAlwaysReadsProfileForPlainValues(t *testing.T) {
	const token = windows.Token(52)
	sid := testSID(t, "S-1-5-21-100-200-300-1001")
	shellFolders := &fakeWindowsUserShellFolders{
		subjectSID: sid,
		values: map[string]fakeRegistryValue{
			roamingAppDataValue: {raw: registryUTF16(`E:\Redirected\Roaming`), valueType: registry.SZ},
			localAppDataValue:   {raw: registryUTF16(`D:\Redirected\Local`), valueType: registry.SZ},
		},
	}
	var profileCalls, expandCalls int
	anchors, err := resolveWindowsAppDataAnchorsWith(
		token,
		shellFolders,
		func(windows.Token) (*windows.SID, error) { return sid, nil },
		func(windows.Token, *uint16, *uint16, uint32) (bool, error) {
			expandCalls++
			return false, nil
		},
		profileProc(t, token, `C:\Users\alice`, &profileCalls),
	)
	want := AppDataAnchors{RoamingAppData: `E:\Redirected\Roaming`, LocalAppData: `D:\Redirected\Local`, UserProfile: `C:\Users\alice`}
	if err != nil || anchors != want {
		t.Fatalf("anchors/error = %#v/%v, want %#v", anchors, err, want)
	}
	if shellFolders.getValueCalls != 8 || shellFolders.subjectCalls != 1 || profileCalls != 2 || expandCalls != 0 {
		t.Fatalf("calls registry/subject/profile/expand = %d/%d/%d/%d", shellFolders.getValueCalls, shellFolders.subjectCalls, profileCalls, expandCalls)
	}
}

func TestResolveWindowsAppDataForSubjectRejectsSubjectMismatchBeforeReads(t *testing.T) {
	tokenSID := testSID(t, "S-1-5-21-100-200-300-1001")
	hiveSID := testSID(t, "S-1-5-21-100-200-300-1002")
	shellFolders := &fakeWindowsUserShellFolders{subjectSID: hiveSID}
	var profileCalls, expandCalls int
	_, err := resolveWindowsAppDataAnchorsWith(
		63,
		shellFolders,
		func(windows.Token) (*windows.SID, error) { return tokenSID, nil },
		func(windows.Token, *uint16, *uint16, uint32) (bool, error) { expandCalls++; return false, nil },
		func(windows.Token, *uint16, *uint32) (bool, error) { profileCalls++; return false, nil },
	)
	if err == nil || !strings.Contains(err.Error(), "subject differ") {
		t.Fatalf("error = %v", err)
	}
	if shellFolders.getValueCalls != 0 || profileCalls != 0 || expandCalls != 0 {
		t.Fatalf("calls registry/profile/expand = %d/%d/%d", shellFolders.getValueCalls, profileCalls, expandCalls)
	}
}

func TestResolveWindowsAppDataForSubjectRejectsInvalidSubjectsBeforeReads(t *testing.T) {
	validSID := testSID(t, "S-1-5-21-100-200-300-1001")
	wantErr := errors.New("subject failed")
	for _, test := range []struct {
		name       string
		tokenSID   *windows.SID
		subjectSID *windows.SID
		subjectErr error
	}{
		{name: "nil token SID", tokenSID: nil, subjectSID: validSID},
		{name: "invalid token SID", tokenSID: new(windows.SID), subjectSID: validSID},
		{name: "nil capability SID", tokenSID: validSID, subjectSID: nil},
		{name: "invalid capability SID", tokenSID: validSID, subjectSID: new(windows.SID)},
		{name: "capability error", tokenSID: validSID, subjectErr: wantErr},
	} {
		t.Run(test.name, func(t *testing.T) {
			shellFolders := &fakeWindowsUserShellFolders{
				subjectSID: test.subjectSID,
				subjectErr: test.subjectErr,
			}
			var profileCalls, expandCalls int
			_, err := resolveWindowsAppDataAnchorsWith(
				63,
				shellFolders,
				func(windows.Token) (*windows.SID, error) { return test.tokenSID, nil },
				func(windows.Token, *uint16, *uint16, uint32) (bool, error) {
					expandCalls++
					return false, nil
				},
				func(windows.Token, *uint16, *uint32) (bool, error) {
					profileCalls++
					return false, nil
				},
			)
			if err == nil {
				t.Fatal("expected invalid subject error")
			}
			if shellFolders.getValueCalls != 0 || profileCalls != 0 || expandCalls != 0 {
				t.Fatalf("calls registry/profile/expand = %d/%d/%d", shellFolders.getValueCalls, profileCalls, expandCalls)
			}
		})
	}
}

func TestResolveWindowsAppDataForSubjectBorrowsCapabilities(t *testing.T) {
	token, closeToken, err := openCurrentUserToken()
	if err != nil {
		t.Fatal(err)
	}
	shellFolders, closeShellFolders, err := openCurrentUserShellFolders(token)
	if err != nil {
		_ = closeToken()
		t.Fatal(err)
	}
	if _, err := ResolveWindowsAppDataForSubject(token, shellFolders); err != nil {
		_ = closeShellFolders()
		_ = closeToken()
		t.Fatal(err)
	}
	if _, err := token.GetTokenUser(); err != nil {
		t.Fatalf("resolver closed borrowed token: %v", err)
	}
	if _, _, err := shellFolders.GetValue(roamingAppDataValue, nil); err != nil {
		t.Fatalf("resolver closed borrowed registry key: %v", err)
	}
	if err := closeShellFolders(); err != nil {
		t.Fatal(err)
	}
	if err := closeToken(); err != nil {
		t.Fatal(err)
	}
}

func TestReadStableRegistryValue(t *testing.T) {
	value := registryUTF16(`C:\Users\alice\AppData\Roaming`)
	reader := &snapshotRegistryReader{values: [][]byte{value, value}, valueTypes: []uint32{registry.SZ, registry.SZ}}
	got, valueType, err := readStableRegistryValue(reader, roamingAppDataValue)
	if err != nil || !bytesEqual(got, value) || valueType != registry.SZ || reader.calls != 4 {
		t.Fatalf("value/type/error/calls = %x/%d/%v/%d", got, valueType, err, reader.calls)
	}
}

func TestReadStableRegistryValueRejectsDrift(t *testing.T) {
	first := registryUTF16(`C:\First`)
	for _, test := range []struct {
		name       string
		second     []byte
		secondType uint32
	}{
		{name: "type", second: first, secondType: registry.EXPAND_SZ},
		{name: "size", second: registryUTF16(`C:\Longer`), secondType: registry.SZ},
		{name: "bytes", second: registryUTF16(`D:\Other`), secondType: registry.SZ},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &snapshotRegistryReader{
				values:     [][]byte{first, test.second},
				valueTypes: []uint32{registry.SZ, test.secondType},
			}
			_, _, err := readStableRegistryValue(reader, roamingAppDataValue)
			if err == nil || !strings.Contains(err.Error(), "changed while reading") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestReadRegistrySnapshotRetriesOneShortBuffer(t *testing.T) {
	value := registryUTF16(`C:\Stable`)
	reader := &scriptedRegistryReader{responses: []registryResponse{
		{n: 4, valueType: registry.SZ},
		{n: len(value), valueType: registry.SZ, err: registry.ErrShortBuffer},
		{n: len(value), valueType: registry.SZ},
		{n: len(value), valueType: registry.SZ, data: value},
	}}
	got, valueType, err := readRegistrySnapshot(reader, roamingAppDataValue)
	if err != nil || !bytesEqual(got, value) || valueType != registry.SZ || reader.index != 4 {
		t.Fatalf("value/type/error/calls = %x/%d/%v/%d", got, valueType, err, reader.index)
	}
}

func TestReadRegistrySnapshotStopsAfterSecondShortBuffer(t *testing.T) {
	reader := &scriptedRegistryReader{responses: []registryResponse{
		{n: 4, valueType: registry.SZ},
		{n: 6, valueType: registry.SZ, err: registry.ErrShortBuffer},
		{n: 8, valueType: registry.EXPAND_SZ},
		{n: 10, valueType: registry.EXPAND_SZ, err: registry.ErrShortBuffer},
	}}
	_, _, err := readRegistrySnapshot(reader, roamingAppDataValue)
	if !errors.Is(err, registry.ErrShortBuffer) || reader.index != 4 {
		t.Fatalf("error/calls = %v/%d", err, reader.index)
	}
}

func TestReadRegistryValueRejectsInvalidProbeSizeBeforeDataRead(t *testing.T) {
	for _, size := range []int{0, 1, 3, maxShellFolderBytes + 2} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			reader := &scriptedRegistryReader{responses: []registryResponse{{n: size, valueType: registry.SZ}}}
			_, _, err := readRegistryValue(reader, roamingAppDataValue)
			if err == nil || reader.index != 1 {
				t.Fatalf("error/calls = %v/%d", err, reader.index)
			}
		})
	}
}

func TestDecodeRegistryUTF16RejectsMalformedValues(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
	}{
		{name: "empty", raw: nil},
		{name: "odd byte count", raw: []byte{1, 0, 0}},
		{name: "missing terminator", raw: rawUTF16('C', ':')},
		{name: "embedded terminator", raw: rawUTF16('C', 0, 'X', 0)},
		{name: "lone high surrogate", raw: rawUTF16(0xd800, 0)},
		{name: "lone low surrogate", raw: rawUTF16(0xdc00, 0)},
		{name: "broken pair", raw: rawUTF16(0xd800, 'X', 0)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got, err := decodeRegistryUTF16(test.raw); err == nil {
				t.Fatalf("decoded malformed value as %q", got)
			}
		})
	}

	want := "C:\\Users\\😀"
	got, err := decodeRegistryUTF16(registryUTF16(want))
	if err != nil || got != want {
		t.Fatalf("decoded = %q, want %q: %v", got, want, err)
	}
}

func TestExpandUserEnvironmentWithUsesOneBoundedCall(t *testing.T) {
	calls := 0
	got, err := expandUserEnvironmentWith(19, `%USERPROFILE%\Data`, func(
		token windows.Token,
		source, destination *uint16,
		size uint32,
	) (bool, error) {
		calls++
		if token != 19 || size != maxAppDataPathUnits || windows.UTF16PtrToString(source) != `%USERPROFILE%\Data` {
			t.Fatalf("token/size/source = %v/%d/%q", token, size, windows.UTF16PtrToString(source))
		}
		writeUTF16(t, destination, size, `C:\Users\alice\Data`)
		return true, nil
	})
	if err != nil || got != `C:\Users\alice\Data` || calls != 1 {
		t.Fatalf("value/error/calls = %q/%v/%d", got, err, calls)
	}
}

func TestExpandUserEnvironmentWithRejectsInvalidOutput(t *testing.T) {
	wantErr := errors.New("expand failed")
	tests := []struct {
		name string
		call expandEnvironmentProc
		want string
	}{
		{
			name: "API failure",
			call: func(windows.Token, *uint16, *uint16, uint32) (bool, error) { return false, wantErr },
			want: "expand failed",
		},
		{
			name: "failure without error",
			call: func(windows.Token, *uint16, *uint16, uint32) (bool, error) { return false, nil },
			want: "without an error",
		},
		{
			name: "missing terminator",
			call: func(_ windows.Token, _, destination *uint16, size uint32) (bool, error) {
				buffer := unsafe.Slice(destination, size)
				for index := range buffer {
					buffer[index] = 'X'
				}
				return true, nil
			},
			want: "not NUL-terminated",
		},
		{
			name: "empty",
			call: func(_ windows.Token, _, destination *uint16, size uint32) (bool, error) {
				unsafe.Slice(destination, size)[0] = 0
				return true, nil
			},
			want: "is empty",
		},
		{
			name: "invalid surrogate",
			call: func(_ windows.Token, _, destination *uint16, size uint32) (bool, error) {
				copy(unsafe.Slice(destination, size), []uint16{0xd800, 0})
				return true, nil
			},
			want: "invalid UTF-16",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := expandUserEnvironmentWith(1, `%X%`, test.call)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestUserProfileWithUsesBoundedTwoCallABI(t *testing.T) {
	calls := 0
	got, err := userProfileWith(7, func(token windows.Token, destination *uint16, size *uint32) (bool, error) {
		calls++
		if token != 7 {
			t.Fatalf("token = %v", token)
		}
		units, err := windows.UTF16FromString(`C:\Users\alice`)
		if err != nil {
			t.Fatal(err)
		}
		if calls == 1 {
			if destination != nil || *size != 0 {
				t.Fatalf("query destination/size = %v/%d", destination, *size)
			}
			*size = uint32(len(units))
			return false, windows.ERROR_INSUFFICIENT_BUFFER
		}
		if destination == nil || *size != uint32(len(units)) {
			t.Fatalf("read destination/size = %v/%d", destination, *size)
		}
		copy(unsafe.Slice(destination, *size), units)
		return true, nil
	})
	if err != nil || got != `C:\Users\alice` || calls != 2 {
		t.Fatalf("profile/error/calls = %q/%v/%d", got, err, calls)
	}
}

func TestUserProfileWithRejectsBrokenABI(t *testing.T) {
	tests := []struct {
		name string
		call userProfileProc
		want string
	}{
		{
			name: "successful nil query",
			call: func(windows.Token, *uint16, *uint32) (bool, error) { return true, nil },
			want: "size query",
		},
		{
			name: "wrong query error",
			call: func(windows.Token, *uint16, *uint32) (bool, error) { return false, windows.ERROR_ACCESS_DENIED },
			want: "access is denied",
		},
		{
			name: "zero size",
			call: func(_ windows.Token, _ *uint16, size *uint32) (bool, error) {
				*size = 0
				return false, windows.ERROR_INSUFFICIENT_BUFFER
			},
			want: "invalid size",
		},
		{
			name: "oversize",
			call: func(_ windows.Token, _ *uint16, size *uint32) (bool, error) {
				*size = maxAppDataPathUnits + 1
				return false, windows.ERROR_INSUFFICIENT_BUFFER
			},
			want: "invalid size",
		},
		{
			name: "changed size",
			call: profileCallSequence(rawUTF16('C', ':', '\\', 0), func(size *uint32) { (*size)-- }),
			want: "not NUL-terminated",
		},
		{
			name: "zero returned count",
			call: profileCallSequence(rawUTF16('C', ':', '\\', 0), func(size *uint32) { *size = 0 }),
			want: "invalid count",
		},
		{
			name: "oversize returned count",
			call: profileCallSequence(rawUTF16('C', ':', '\\', 0), func(size *uint32) { (*size)++ }),
			want: "invalid count",
		},
		{
			name: "missing terminator",
			call: profileCallSequence(rawUTF16('C', ':', '\\'), func(size *uint32) {}),
			want: "not NUL-terminated",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := userProfileWith(1, test.call)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.want)) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestAppDataPathWithValidatesTypeExpansionAndPath(t *testing.T) {
	wantErr := errors.New("sentinel")
	tests := []struct {
		name      string
		raw       []byte
		valueType uint32
		readErr   error
		expanded  string
		expandErr error
		want      string
		wantErr   string
	}{
		{name: "plain", raw: registryUTF16(`C:\Data`), valueType: registry.SZ, want: `C:\Data`},
		{name: "expanded", raw: registryUTF16(`%USERPROFILE%\Data`), valueType: registry.EXPAND_SZ, expanded: `D:\Data`, want: `D:\Data`},
		{name: "read error", readErr: wantErr, wantErr: "sentinel"},
		{name: "unsupported type", raw: registryUTF16(`C:\Data`), valueType: registry.BINARY, wantErr: "unsupported registry type"},
		{name: "malformed raw", raw: rawUTF16('C', ':'), valueType: registry.SZ, wantErr: "not NUL-terminated"},
		{name: "expand error", raw: registryUTF16(`%X%`), valueType: registry.EXPAND_SZ, expandErr: wantErr, wantErr: "sentinel"},
		{name: "empty", raw: registryUTF16(``), valueType: registry.SZ, wantErr: "is empty"},
		{name: "relative", raw: registryUTF16(`relative`), valueType: registry.SZ, wantErr: "clean absolute"},
		{name: "drive relative", raw: registryUTF16(`C:relative`), valueType: registry.SZ, wantErr: "local drive"},
		{name: "UNC", raw: registryUTF16(`\\server\share\Data`), valueType: registry.SZ, wantErr: "local drive"},
		{name: "extended namespace", raw: registryUTF16(`\\?\C:\Data`), valueType: registry.SZ, wantErr: "local drive"},
		{name: "device namespace", raw: registryUTF16(`\\.\C:\Data`), valueType: registry.SZ, wantErr: "local drive"},
		{name: "alternate data stream", raw: registryUTF16(`C:\Data:stream`), valueType: registry.SZ, wantErr: "local drive"},
		{name: "unclean", raw: registryUTF16(`C:\Data\..\Other`), valueType: registry.SZ, wantErr: "clean absolute"},
		{name: "expanded over bound", raw: registryUTF16(`%X%`), valueType: registry.EXPAND_SZ, expanded: `C:\` + strings.Repeat("x", maxAppDataPathUnits), wantErr: "path limit"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			expandCalls := 0
			got, err := appDataPathWith(
				roamingAppDataValue,
				func(name string) ([]byte, uint32, error) {
					if name != roamingAppDataValue {
						t.Fatalf("registry name = %q", name)
					}
					return test.raw, test.valueType, test.readErr
				},
				func(string) (string, error) {
					expandCalls++
					return test.expanded, test.expandErr
				},
			)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("value/error = %q/%v, want error %q", got, err, test.wantErr)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("value/error = %q/%v, want %q", got, err, test.want)
			}
			wantExpandCalls := 0
			if test.valueType == registry.EXPAND_SZ {
				wantExpandCalls = 1
			}
			if expandCalls != wantExpandCalls {
				t.Fatalf("expand calls = %d, want %d", expandCalls, wantExpandCalls)
			}
		})
	}
}

func TestWindowsAppDataAnchorsRejectsInvalidProfileBeforeReads(t *testing.T) {
	for _, test := range []struct {
		name    string
		profile string
	}{
		{name: "relative", profile: `relative`},
		{name: "UNC", profile: `\\server\share\profile`},
		{name: "extended namespace", profile: `\\?\C:\Users\alice`},
		{name: "device namespace", profile: `\\.\C:\Users\alice`},
		{name: "alternate data stream", profile: `C:\Users\alice:stream`},
		{name: "non-clean", profile: `C:\Users\alice\..\bob`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var readCalls, expandCalls int
			_, err := windowsAppDataAnchorsWith(
				func(string) ([]byte, uint32, error) {
					readCalls++
					return nil, 0, nil
				},
				func(string) (string, error) {
					expandCalls++
					return "", nil
				},
				test.profile,
			)
			if err == nil || readCalls != 0 || expandCalls != 0 {
				t.Fatalf("error/read/expand calls = %v/%d/%d", err, readCalls, expandCalls)
			}
		})
	}
}

func TestWindowsAppDataAnchorsAreAbsolute(t *testing.T) {
	anchors, err := WindowsAppDataAnchors()
	if err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"roaming": anchors.RoamingAppData,
		"local":   anchors.LocalAppData,
		"profile": anchors.UserProfile,
	} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			t.Fatalf("%s path is not clean and absolute: %q", name, path)
		}
	}
}

type registryResponse struct {
	n         int
	valueType uint32
	data      []byte
	err       error
}

type fakeRegistryValue struct {
	raw       []byte
	valueType uint32
}

type fakeRegistryKey struct {
	closeCalls int
}

func (*fakeRegistryKey) GetValue(string, []byte) (int, uint32, error) {
	return 0, 0, registry.ErrNotExist
}

func (key *fakeRegistryKey) Close() error {
	key.closeCalls++
	return nil
}

type fakeWindowsUserShellFolders struct {
	subjectSID    *windows.SID
	subjectErr    error
	values        map[string]fakeRegistryValue
	getValueCalls int
	subjectCalls  int
}

func (shellFolders *fakeWindowsUserShellFolders) GetValue(
	name string,
	buffer []byte,
) (int, uint32, error) {
	shellFolders.getValueCalls++
	value, ok := shellFolders.values[name]
	if !ok {
		return 0, 0, registry.ErrNotExist
	}
	copy(buffer, value.raw)
	return len(value.raw), value.valueType, nil
}

func (shellFolders *fakeWindowsUserShellFolders) SubjectUserSID() (*windows.SID, error) {
	shellFolders.subjectCalls++
	return shellFolders.subjectSID, shellFolders.subjectErr
}

type scriptedRegistryReader struct {
	responses []registryResponse
	index     int
}

func (reader *scriptedRegistryReader) GetValue(_ string, buffer []byte) (int, uint32, error) {
	if reader.index >= len(reader.responses) {
		return 0, 0, errors.New("unexpected registry read")
	}
	response := reader.responses[reader.index]
	reader.index++
	copy(buffer, response.data)
	return response.n, response.valueType, response.err
}

type snapshotRegistryReader struct {
	values     [][]byte
	valueTypes []uint32
	calls      int
}

func (reader *snapshotRegistryReader) GetValue(_ string, buffer []byte) (int, uint32, error) {
	snapshot := reader.calls / 2
	reader.calls++
	if snapshot >= len(reader.values) {
		return 0, 0, errors.New("unexpected registry read")
	}
	value := reader.values[snapshot]
	copy(buffer, value)
	return len(value), reader.valueTypes[snapshot], nil
}

func registryUTF16(value string) []byte {
	units, err := windows.UTF16FromString(value)
	if err != nil {
		panic(err)
	}
	return rawUTF16(units...)
}

func rawUTF16(units ...uint16) []byte {
	data := make([]byte, len(units)*2)
	for index, unit := range units {
		binary.LittleEndian.PutUint16(data[index*2:], unit)
	}
	return data
}

func writeUTF16(t *testing.T, destination *uint16, size uint32, value string) {
	t.Helper()
	units, err := windows.UTF16FromString(value)
	if err != nil {
		t.Fatal(err)
	}
	if len(units) > int(size) {
		t.Fatalf("value needs %d units, buffer has %d", len(units), size)
	}
	copy(unsafe.Slice(destination, size), units)
}

func profileProc(
	t *testing.T,
	wantToken windows.Token,
	profile string,
	calls *int,
) userProfileProc {
	t.Helper()
	return func(token windows.Token, destination *uint16, size *uint32) (bool, error) {
		*calls++
		if token != wantToken {
			t.Fatalf("profile token = %v", token)
		}
		units, err := windows.UTF16FromString(profile)
		if err != nil {
			t.Fatal(err)
		}
		if destination == nil {
			if *size != 0 {
				t.Fatalf("initial profile size = %d", *size)
			}
			*size = uint32(len(units))
			return false, windows.ERROR_INSUFFICIENT_BUFFER
		}
		if *size != uint32(len(units)) {
			t.Fatalf("profile buffer size = %d", *size)
		}
		copy(unsafe.Slice(destination, *size), units)
		return true, nil
	}
}

func testSID(t *testing.T, value string) *windows.SID {
	t.Helper()
	sid, err := windows.StringToSid(value)
	if err != nil {
		t.Fatal(err)
	}
	return sid
}

func profileCallSequence(data []byte, mutateSize func(*uint32)) userProfileProc {
	calls := 0
	return func(_ windows.Token, destination *uint16, size *uint32) (bool, error) {
		calls++
		if calls == 1 {
			*size = uint32(len(data) / 2)
			return false, windows.ERROR_INSUFFICIENT_BUFFER
		}
		units := make([]uint16, len(data)/2)
		for index := range units {
			units[index] = binary.LittleEndian.Uint16(data[index*2:])
		}
		copy(unsafe.Slice(destination, *size), units)
		mutateSize(size)
		return true, nil
	}
}

func bytesEqual(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
