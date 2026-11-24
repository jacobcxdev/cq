package main

import (
	"bytes"
	"debug/pe"
	_ "embed"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

//go:embed console_windows.manifest
var windowsConsoleManifest []byte

func TestWindowsCQConsoleAllocationManifest(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	image, err := pe.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	defer image.Close()
	header, ok := image.OptionalHeader.(*pe.OptionalHeader64)
	if !ok || header.Subsystem != pe.IMAGE_SUBSYSTEM_WINDOWS_CUI {
		t.Fatal("Windows CQ must remain a console executable for interactive shells")
	}
	resource, err := windows.FindResource(0, windows.CREATEPROCESS_MANIFEST_RESOURCE_ID, windows.RT_MANIFEST)
	if err != nil {
		t.Fatalf("Windows CQ has no application manifest: %v", err)
	}
	data, err := windows.LoadResourceData(0, resource)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(data), bytes.TrimSpace(windowsConsoleManifest)) {
		t.Fatal("Windows CQ has no current detached console manifest")
	}
}
