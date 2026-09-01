//go:build !windows

package main

import "strings"

func windowsTaskUserMatchesSID(userID, sid string) bool {
	return strings.EqualFold(userID, sid)
}
