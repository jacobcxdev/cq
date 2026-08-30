//go:build windows

package userdirs

import (
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func (resolver Resolver) Resolve() (Roots, error) {
	if resolver.RoamingAppData == nil || resolver.LocalAppData == nil {
		return Roots{}, fmt.Errorf("resolve CQ user directories: incomplete resolver")
	}

	configBase, err := resolver.RoamingAppData()
	if err != nil {
		return Roots{}, fmt.Errorf("resolve Windows roaming data: %w", err)
	}
	if err := validateWindowsLocalAbsolutePath("Windows roaming data", configBase); err != nil {
		return Roots{}, err
	}

	localBase, err := resolver.LocalAppData()
	if err != nil {
		return Roots{}, fmt.Errorf("resolve Windows local data: %w", err)
	}
	if err := validateWindowsLocalAbsolutePath("Windows local data", localBase); err != nil {
		return Roots{}, err
	}

	config := filepath.Join(configBase, "cq")
	local := filepath.Join(localBase, "cq")
	roots := Roots{
		Config:  config,
		State:   filepath.Join(local, "state"),
		Cache:   filepath.Join(local, "cache"),
		Runtime: filepath.Join(local, "runtime"),
		Logs:    filepath.Join(local, "logs"),
	}
	for _, root := range []string{roots.Config, roots.State, roots.Cache, roots.Runtime, roots.Logs} {
		if !filepath.IsAbs(root) {
			return Roots{}, fmt.Errorf("CQ root is not absolute: %q", root)
		}
	}
	return roots, nil
}

func validateWindowsLocalAbsolutePath(label, path string) error {
	volume := filepath.VolumeName(path)
	driveLetter := len(volume) == 2 && volume[1] == ':' && isASCIILetter(volume[0])
	if !driveLetter || len(path) < 3 || path[2] != '\\' || !filepath.IsAbs(path) ||
		filepath.Clean(path) != path || strings.IndexByte(path, 0) >= 0 || strings.Contains(path[2:], ":") {
		return fmt.Errorf("%s is not a clean absolute local drive path", label)
	}
	return nil
}

func isASCIILetter(value byte) bool {
	return ('A' <= value && value <= 'Z') || ('a' <= value && value <= 'z')
}

type AppDataAnchors struct {
	RoamingAppData string
	LocalAppData   string
	UserProfile    string
}

type WindowsUserShellFolders interface {
	GetValue(name string, buffer []byte) (n int, valueType uint32, err error)
	SubjectUserSID() (*windows.SID, error)
}

func ResolveWindowsAppDataForSubject(
	token windows.Token,
	shellFolders WindowsUserShellFolders,
) (AppDataAnchors, error) {
	return resolveWindowsAppDataAnchors(token, shellFolders)
}

func WindowsAppDataAnchors() (AppDataAnchors, error) {
	return currentUserAppDataAnchors()
}

func Default() (Roots, error) {
	anchors, err := WindowsAppDataAnchors()
	if err != nil {
		return Roots{}, err
	}
	return (Resolver{
		RoamingAppData: func() (string, error) { return anchors.RoamingAppData, nil },
		LocalAppData:   func() (string, error) { return anchors.LocalAppData, nil },
	}).Resolve()
}
