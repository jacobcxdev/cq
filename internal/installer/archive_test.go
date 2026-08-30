package installer

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

type archiveTestEntry struct {
	name     string
	body     []byte
	mode     fs.FileMode
	tarType  byte
	linkName string
}

func TestExtractReleaseArchiveZIPAndTarGzip(t *testing.T) {
	tests := []struct {
		name       string
		archive    func(*testing.T, []archiveTestEntry) []byte
		assetName  string
		executable string
	}{
		{name: "zip", archive: makeZIPArchive, assetName: "cq_0.27.0_windows_amd64.zip", executable: "cq.exe"},
		{name: "tar gzip", archive: makeTarGzipArchive, assetName: "cq_0.27.0_linux_arm64.tar.gz", executable: "cq"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			staged, err := extractReleaseArchive(test.archive(t, []archiveTestEntry{{name: test.executable, body: []byte("cq"), mode: 0o755}}), test.assetName, t.TempDir(), test.executable)
			if err != nil {
				t.Fatal(err)
			}
			if filepath.Base(staged.Path) != test.executable {
				t.Fatalf("staged path = %q", staged.Path)
			}
			info, err := os.Stat(staged.Path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm()&0o700 != 0o700 {
				t.Fatalf("staged mode = %o", info.Mode().Perm())
			}
		})
	}
}

func TestExtractReleaseArchiveRejectsUnsafeZIPEntries(t *testing.T) {
	tests := []struct {
		name    string
		entries []archiveTestEntry
	}{
		{name: "absolute", entries: []archiveTestEntry{{name: "/cq.exe", body: []byte("cq"), mode: 0o700}}},
		{name: "drive absolute", entries: []archiveTestEntry{{name: `C:\cq.exe`, body: []byte("cq"), mode: 0o700}}},
		{name: "traversal", entries: []archiveTestEntry{{name: "../cq.exe", body: []byte("cq"), mode: 0o700}}},
		{name: "symlink", entries: []archiveTestEntry{{name: "cq.exe", body: []byte("target"), mode: os.ModeSymlink | 0o777}}},
		{name: "duplicate", entries: []archiveTestEntry{{name: "cq.exe", body: []byte("one"), mode: 0o700}, {name: "cq.exe", body: []byte("two"), mode: 0o700}}},
		{name: "unexpected", entries: []archiveTestEntry{{name: "README", body: []byte("readme"), mode: 0o600}}},
		{name: "extra executable", entries: []archiveTestEntry{{name: "cq.exe", body: []byte("cq"), mode: 0o700}, {name: "helper.exe", body: []byte("helper"), mode: 0o700}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			destination := t.TempDir()
			if _, err := extractReleaseArchive(makeZIPArchive(t, test.entries), "cq.zip", destination, "cq.exe"); err == nil {
				t.Fatal("unsafe ZIP accepted")
			}
			if _, err := os.Stat(filepath.Join(destination, "cq.exe")); err == nil {
				t.Fatal("failed extraction left executable")
			}
		})
	}
}

func TestExtractReleaseArchiveRejectsUnsafeTarEntries(t *testing.T) {
	tests := []struct {
		name    string
		entries []archiveTestEntry
	}{
		{name: "absolute", entries: []archiveTestEntry{{name: "/cq", body: []byte("cq"), mode: 0o700}}},
		{name: "traversal", entries: []archiveTestEntry{{name: "../cq", body: []byte("cq"), mode: 0o700}}},
		{name: "symlink", entries: []archiveTestEntry{{name: "cq", tarType: tar.TypeSymlink, linkName: "target", mode: 0o777}}},
		{name: "hard link", entries: []archiveTestEntry{{name: "cq", tarType: tar.TypeLink, linkName: "target", mode: 0o777}}},
		{name: "device", entries: []archiveTestEntry{{name: "cq", tarType: tar.TypeChar, mode: 0o700}}},
		{name: "duplicate", entries: []archiveTestEntry{{name: "cq", body: []byte("one"), mode: 0o700}, {name: "cq", body: []byte("two"), mode: 0o700}}},
		{name: "unexpected", entries: []archiveTestEntry{{name: "README", body: []byte("readme"), mode: 0o600}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			destination := t.TempDir()
			if _, err := extractReleaseArchive(makeTarGzipArchive(t, test.entries), "cq.tar.gz", destination, "cq"); err == nil {
				t.Fatal("unsafe tar.gz accepted")
			}
			if _, err := os.Stat(filepath.Join(destination, "cq")); err == nil {
				t.Fatal("failed extraction left executable")
			}
		})
	}
}

func TestExtractReleaseArchiveRejectsOversizedTarEntry(t *testing.T) {
	destination := t.TempDir()
	archive := makeOversizedTarGzipArchive(t, "cq", maxExtractedExecutableBytes+1)
	if _, err := extractReleaseArchive(archive, "cq.tar.gz", destination, "cq"); err == nil {
		t.Fatal("oversized tar entry accepted")
	}
	if _, err := os.Stat(filepath.Join(destination, "cq")); err == nil {
		t.Fatal("oversized extraction left executable")
	}
}

func TestExtractReleaseArchiveRejectsUnknownFormatAndInvalidDestination(t *testing.T) {
	if _, err := extractReleaseArchive([]byte("cq"), "cq.exe", t.TempDir(), "cq"); err == nil {
		t.Fatal("unknown archive format accepted")
	}
	notDirectory := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notDirectory, []byte("cq"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := extractReleaseArchive(makeZIPArchive(t, []archiveTestEntry{{name: "cq.exe", body: []byte("cq"), mode: 0o700}}), "cq.zip", notDirectory, "cq.exe"); err == nil {
		t.Fatal("file destination accepted")
	}
}

func TestExtractReleaseArchiveRejectsCorruptGzipAndCleansStagedFile(t *testing.T) {
	archive := makeTarGzipArchive(t, []archiveTestEntry{{name: "cq", body: []byte("cq"), mode: 0o700}})
	archive[len(archive)-1] ^= 0xff
	destination := t.TempDir()
	if _, err := extractReleaseArchive(archive, "cq.tar.gz", destination, "cq"); err == nil {
		t.Fatal("corrupt gzip accepted")
	}
	if _, err := os.Stat(filepath.Join(destination, "cq")); err == nil {
		t.Fatal("corrupt extraction left executable")
	}
}

func makeZIPArchive(t *testing.T, entries []archiveTestEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Store}
		header.SetMode(entry.mode)
		file, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func makeTarGzipArchive(t *testing.T, entries []archiveTestEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, entry := range entries {
		typeFlag := entry.tarType
		if typeFlag == 0 {
			typeFlag = tar.TypeReg
		}
		header := &tar.Header{Name: entry.name, Mode: int64(entry.mode.Perm()), Size: int64(len(entry.body)), Typeflag: typeFlag, Linkname: entry.linkName}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if len(entry.body) > 0 {
			if _, err := tarWriter.Write(entry.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func makeOversizedTarGzipArchive(t *testing.T, name string, size int64) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0o700, Size: size, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	// Deliberately omit body and tar trailer. Extractor must reject declared
	// size before reading either.
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
