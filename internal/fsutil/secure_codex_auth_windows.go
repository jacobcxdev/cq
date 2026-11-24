//go:build windows

package fsutil

import (
	"fmt"
	"strings"

	"golang.org/x/sys/windows"
)

// ValidateCodexSystemCredential permits only the local Codex sandbox group's
// read/execute ACE in addition to trusted Windows principals on a system auth
// file. Callers must restrict this exception to the exact Codex auth path.
func (OSFileSystem) ValidateCodexSystemCredential(file SecureReadFile) error {
	opened, ok := file.(*windowsSecureFile)
	if !ok {
		return ErrSecureCapabilityUnavailable
	}
	info, err := opened.Stat()
	if err != nil {
		return err
	}
	if err := ValidateExternalCacheFile(OSFileSystem{}, info); err != nil {
		return err
	}
	computer, err := windows.ComputerName()
	if err != nil {
		return err
	}
	sandboxSID, domain, kind, err := windows.LookupSID("", computer+`\CodexSandboxUsers`)
	if err != nil || !strings.EqualFold(domain, computer) || kind != windows.SidTypeAlias {
		return fmt.Errorf("%w: local Codex sandbox group", ErrUnsafeSecurePath)
	}
	sandbox, ok := windowsPrincipal(sandboxSID)
	if !ok {
		return ErrUnsafeSecurePath
	}
	descriptor, err := windows.GetSecurityInfo(windows.Handle(opened.file.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	control, _, err := descriptor.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return fmt.Errorf("%w: Codex system credential DACL", ErrUnsafeSecurePath)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	aces, err := parseWindowsACL(dacl)
	if err != nil {
		return err
	}
	userSID, err := currentWindowsUserSID()
	if err != nil {
		return err
	}
	user, userOK := windowsPrincipal(userSID)
	systemSID, systemErr := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	adminSID, adminErr := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	system, systemOK := windowsPrincipal(systemSID)
	admin, adminOK := windowsPrincipal(adminSID)
	if !userOK || systemErr != nil || adminErr != nil || !systemOK || !adminOK {
		return ErrUnsafeSecurePath
	}
	allowed := uint32(windows.FILE_GENERIC_READ | windows.FILE_GENERIC_EXECUTE | windows.GENERIC_READ | windows.GENERIC_EXECUTE)
	sandboxCount := 0
	for _, ace := range aces {
		if ace.Principal == user || ace.Principal == system || ace.Principal == admin {
			continue
		}
		if ace.Principal != sandbox || ace.Mask == 0 || ace.Mask&^allowed != 0 {
			return fmt.Errorf("%w: Codex system credential reader", ErrUnsafeSecurePath)
		}
		sandboxCount++
	}
	if sandboxCount != 1 {
		return fmt.Errorf("%w: Codex system credential sandbox ACE", ErrUnsafeSecurePath)
	}
	return nil
}
