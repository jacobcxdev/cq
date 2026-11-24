//go:build windows

package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/dots"
)

func TestWindowsDotsCloudEntryRecognisesOnlyStagedHelper(t *testing.T) {
	root := filepath.Join(t.TempDir(), "dots")
	helper := dots.WindowsCloudWrapperPath(root)
	if !isWindowsDotsCloudHelper(strings.ToUpper(helper), root, []string{"--version"}) ||
		!isWindowsDotsCloudHelper(helper, root, []string{"app-server"}) {
		t.Fatal("staged executable did not select the Dots cloud entry")
	}
	if isWindowsDotsCloudHelper(filepath.Join(root, "cq.exe"), root, []string{"--version"}) ||
		isWindowsDotsCloudHelper(helper, filepath.Dir(root), []string{"app-server"}) ||
		isWindowsDotsCloudHelper(helper, root, []string{"__dots", "native-broker", root}) ||
		isWindowsDotsCloudHelper(helper, "relative", []string{"app-server"}) {
		t.Fatal("non-wrapper or private helper invocation selected the Dots cloud entry")
	}
}
