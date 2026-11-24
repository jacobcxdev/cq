//go:build !windows

package main

import "io"

func runWindowsDotsCloudEntry([]string, io.Reader, io.Writer, io.Writer) (bool, error) {
	return false, nil
}
