//go:build !unix

package codex

import "os"

func ownedByCurrentUser(os.FileInfo) bool { return false }
