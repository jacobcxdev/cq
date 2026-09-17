//go:build windows

package userdirs

import (
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func (resolver Resolver) Resolve(wanted ...Root) (Roots, error) {
	var roots Roots
	if selected(wanted, ConfigRoot) {
		if resolver.RoamingAppData == nil {
			return Roots{}, rootUnavailable()
		}
		base, err := resolver.RoamingAppData()
		if err != nil {
			return Roots{}, rootUnavailable()
		}
		if err := validateWindowsLocalAbsolutePath("Windows roaming data", base); err != nil {
			return Roots{}, rootUnavailable()
		}
		roots.Config = filepath.Join(base, "cq")
	}
	if selected(wanted, StateRoot) || selected(wanted, CacheRoot) || selected(wanted, RuntimeRoot) || selected(wanted, LogsRoot) {
		if resolver.LocalAppData == nil {
			return Roots{}, rootUnavailable()
		}
		base, err := resolver.LocalAppData()
		if err != nil {
			return Roots{}, rootUnavailable()
		}
		if err := validateWindowsLocalAbsolutePath("Windows local data", base); err != nil {
			return Roots{}, rootUnavailable()
		}
		base = filepath.Join(base, "cq")
		if selected(wanted, StateRoot) {
			roots.State = filepath.Join(base, "state")
		}
		if selected(wanted, CacheRoot) {
			roots.Cache = filepath.Join(base, "cache")
		}
		if selected(wanted, RuntimeRoot) {
			roots.Runtime = filepath.Join(base, "runtime")
		}
		if selected(wanted, LogsRoot) {
			roots.Logs = filepath.Join(base, "logs")
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

func Default(wanted ...Root) (Roots, error) {
	anchors, err := WindowsAppDataAnchors()
	if err != nil {
		return Roots{}, rootUnavailable()
	}
	return (Resolver{
		RoamingAppData: func() (string, error) { return anchors.RoamingAppData, nil },
		LocalAppData:   func() (string, error) { return anchors.LocalAppData, nil },
	}).Resolve(wanted...)
}

// UserHomeDir uses the same authenticated subject as CQ app-data roots.
func UserHomeDir() (string, error) {
	anchors, err := WindowsAppDataAnchors()
	if err != nil {
		return "", rootUnavailable()
	}
	return anchors.UserProfile, nil
}
