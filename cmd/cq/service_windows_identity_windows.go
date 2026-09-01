//go:build windows

package main

import (
	"strings"

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
