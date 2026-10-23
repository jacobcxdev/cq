//go:build windows

package userdirs

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Exercise Default/UserHomeDir through real capability acquisition and folder
// resolution, replacing only native token/registry/ABI operations with fakes.
func TestWindowsProductionSelectedAnchors(t *testing.T) {
	cases := []struct {
		name  string
		roots []Root
		home  bool
		value string
	}{
		{name: "config", roots: []Root{ConfigRoot}, value: roamingAppDataValue},
		{name: "state", roots: []Root{StateRoot}, value: localAppDataValue},
		{name: "cache", roots: []Root{CacheRoot}, value: localAppDataValue},
		{name: "runtime", roots: []Root{RuntimeRoot}, value: localAppDataValue},
		{name: "logs", roots: []Root{LogsRoot}, value: localAppDataValue},
		{name: "profile", home: true},
	}
	for _, tc := range cases {
		for _, failSelected := range []bool{false, true} {
			name := tc.name
			if failSelected {
				name += "/selected_unavailable"
			}
			t.Run(name, func(t *testing.T) {
				const token = windows.Token(87)
				sid := testSID(t, "S-1-5-21-100-200-300-1001")
				folders := &fakeWindowsUserShellFolders{subjectSID: sid, values: map[string]fakeRegistryValue{}}
				if !failSelected && !tc.home {
					folders.values[tc.value] = fakeRegistryValue{raw: registryUTF16(`%USERPROFILE%\Selected`), valueType: registry.EXPAND_SZ}
				}
				var opens, closes, shellOpens, shellCloses, sidReads, profileCalls, expandCalls int
				old := resolveCurrentWindowsFolders
				t.Cleanup(func() { resolveCurrentWindowsFolders = old })
				resolveCurrentWindowsFolders = func(wanted windowsFolderSelection) (AppDataAnchors, error) {
					return currentUserAppDataAnchorsWith(
						func() (windows.Token, func() error, error) {
							opens++
							return token, func() error { closes++; return nil }, nil
						},
						func(got windows.Token) (WindowsUserShellFolders, func() error, error) {
							shellOpens++
							if tc.home || got != token {
								t.Fatal("unrelated registry acquisition or wrong token")
							}
							return folders, func() error { shellCloses++; return nil }, nil
						},
						func(got windows.Token, shell WindowsUserShellFolders) (AppDataAnchors, error) {
							return resolveWindowsAppDataAnchorsWith(got, shell,
								func(got windows.Token) (*windows.SID, error) {
									sidReads++
									if got != token {
										t.Fatal("wrong SID token")
									}
									return sid, nil
								},
								func(got windows.Token, source, destination *uint16, size uint32) (bool, error) {
									expandCalls++
									if tc.home || got != token || windows.UTF16PtrToString(source) != `%USERPROFILE%\Selected` {
										t.Fatal("unrelated expansion or wrong subject")
									}
									writeUTF16(t, destination, size, `C:\Subject\Selected`)
									return true, nil
								},
								func(got windows.Token, destination *uint16, size *uint32) (bool, error) {
									if !tc.home {
										t.Fatal("unrelated profile read")
									}
									if failSelected {
										profileCalls++
										return false, windows.ERROR_ACCESS_DENIED
									}
									return profileProc(t, token, `C:\Subject`, &profileCalls)(got, destination, size)
								}, wanted)
						}, wanted)
				}
				var err error
				if tc.home {
					var home string
					home, err = UserHomeDir()
					if !failSelected && home != `C:\Subject` {
						t.Fatalf("home = %q", home)
					}
				} else {
					var roots Roots
					roots, err = Default(tc.roots...)
					if !failSelected {
						got := map[Root]string{ConfigRoot: roots.Config, StateRoot: roots.State, CacheRoot: roots.Cache, RuntimeRoot: roots.Runtime, LogsRoot: roots.Logs}[tc.roots[0]]
						if got == "" {
							t.Fatal("selected root missing")
						}
					}
				}
				if failSelected {
					var diagnostic *EnvironmentError
					if !errors.As(err, &diagnostic) || diagnostic.Code != "environment_root_unavailable" || diagnostic.ExitCode != 4 {
						t.Fatalf("error = %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				if opens != 1 || closes != 1 || sidReads != 1 {
					t.Fatalf("token opens/closes/SID reads = %d/%d/%d", opens, closes, sidReads)
				}
				if tc.home {
					wantProfile := 2
					if failSelected {
						wantProfile = 1
					}
					if shellOpens != 0 || shellCloses != 0 || folders.getValueCalls != 0 || folders.subjectCalls != 0 || expandCalls != 0 || profileCalls != wantProfile {
						t.Fatal("profile resolution consulted unrelated inputs")
					}
				} else {
					wantReads, wantExpand := 4, 1
					if failSelected {
						wantReads, wantExpand = 1, 0
					}
					if shellOpens != 1 || shellCloses != 1 || folders.subjectCalls != 1 || profileCalls != 0 || folders.getValueCalls != wantReads || expandCalls != wantExpand {
						t.Fatalf("shell %d/%d subject %d profile %d reads %d expand %d", shellOpens, shellCloses, folders.subjectCalls, profileCalls, folders.getValueCalls, expandCalls)
					}
					for _, name := range folders.getValueNames {
						if name != tc.value {
							t.Fatalf("read unrelated folder %q", name)
						}
					}
				}
			})
		}
	}
}

func TestWindowsProfileAcquisitionClosesTokenOnFailure(t *testing.T) {
	resolveErr, closeErr := errors.New("profile failed"), errors.New("close failed")
	for _, failResolve := range []bool{false, true} {
		calls := 0
		_, err := currentUserAppDataAnchorsWith(
			func() (windows.Token, func() error, error) { return 87, func() error { calls++; return closeErr }, nil },
			func(windows.Token) (WindowsUserShellFolders, func() error, error) {
				t.Fatal("opened unrelated registry")
				return nil, nil, nil
			},
			func(windows.Token, WindowsUserShellFolders) (AppDataAnchors, error) {
				if failResolve {
					return AppDataAnchors{}, resolveErr
				}
				return AppDataAnchors{UserProfile: `C:\Subject`}, nil
			},
			windowsFolderSelection{profile: true},
		)
		if calls != 1 || !errors.Is(err, closeErr) || errors.Is(err, resolveErr) != failResolve {
			t.Fatalf("closes/error = %d/%v", calls, err)
		}
	}
}

func TestWindowsSelectedAnchorsRejectInvalidSubjectsBeforeReads(t *testing.T) {
	valid := testSID(t, "S-1-5-21-100-200-300-1001")
	other := testSID(t, "S-1-5-21-100-200-300-1002")
	for _, tc := range []struct {
		name                  string
		wanted                windowsFolderSelection
		tokenSID, registrySID *windows.SID
	}{
		{"profile nil SID", windowsFolderSelection{profile: true}, nil, nil},
		{"profile invalid SID", windowsFolderSelection{profile: true}, new(windows.SID), nil},
		{"config wrong registry subject", windowsFolderSelection{roaming: true}, valid, other},
		{"cache missing registry subject", windowsFolderSelection{local: true}, valid, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			folders := &fakeWindowsUserShellFolders{subjectSID: tc.registrySID}
			_, err := resolveWindowsAppDataAnchorsWith(87, folders,
				func(windows.Token) (*windows.SID, error) { return tc.tokenSID, nil },
				func(windows.Token, *uint16, *uint16, uint32) (bool, error) {
					t.Fatal("expanded before subject validation")
					return false, nil
				},
				func(windows.Token, *uint16, *uint32) (bool, error) {
					t.Fatal("read profile before subject validation")
					return false, nil
				},
				tc.wanted,
			)
			if err == nil || folders.getValueCalls != 0 {
				t.Fatalf("error/reads = %v/%d", err, folders.getValueCalls)
			}
		})
	}
}
