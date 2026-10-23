//go:build windows

package main

import (
	"fmt"
	"strings"
	"unsafe"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"golang.org/x/sys/windows"
)

func windowsTaskUserMatchesSID(userID, sid string) bool {
	if strings.EqualFold(userID, sid) {
		return true
	}
	actual, _, _, err := windows.LookupSID("", userID)
	if err != nil {
		return false
	}
	expected, err := windows.StringToSid(sid)
	return err == nil && actual.Equals(expected)
}

func validateWindowsServiceExecutable(path string) error {
	file, err := (fsutil.OSFileSystem{}).OpenRetainedRegularFileNoFollow(path, fsutil.RetainedRegularFileExecutableDenyReplacement)
	if err != nil {
		return err
	}
	return file.Close()
}
func readWindowsServiceProcessIdentity(pid uint32) (windowsServiceProcessIdentity, error) {
	identity := windowsServiceProcessIdentity{PID: pid}
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return identity, err
	}
	defer windows.CloseHandle(process)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(process, &created, &exited, &kernel, &user); err != nil {
		return identity, err
	}
	if exited.HighDateTime != 0 || exited.LowDateTime != 0 {
		return identity, fmt.Errorf("process already exited")
	}
	identity.Created = uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime)
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return identity, err
	}
	defer token.Close()
	subject, err := token.GetTokenUser()
	if err != nil {
		return identity, err
	}
	identity.SID = subject.User.Sid.String()
	buffer := make([]uint16, 32768)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(process, 0, &buffer[0], &size); err != nil {
		return identity, err
	}
	identity.Executable = windows.UTF16ToString(buffer[:size])
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return identity, err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	found := false
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		if entry.ParentProcessID == pid {
			identity.Children++
			identity.OnlyChildPID = entry.ProcessID
		}
		if entry.ProcessID == pid {
			identity.ParentPID = entry.ParentProcessID
			found = true
		}
	}
	if err != windows.ERROR_NO_MORE_FILES {
		return identity, err
	}
	if found {
		return identity, nil
	}
	return identity, fmt.Errorf("process identity disappeared")
}
