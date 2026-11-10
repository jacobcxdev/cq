package fsutil

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestMatchSecureFileNativeRejectsUnsafeEntries(t *testing.T) {
	requireUnixSecureFS(t)
	fsy := OSFileSystem{}
	root := filepath.Join(t.TempDir(), "state")
	if err := EnsureSecureDirectory(fsy, root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "value")
	want := bytes.Repeat([]byte("x"), 65537)
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	directory, err := fsy.OpenSecureDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	identity, _ := fsy.FileIdentity(info)
	if err := MatchSecureFileInDirectoryWithIdentity(fsy, directory, "value", want, identity); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := MatchSecureFileInDirectoryWithIdentity(fsy, directory, "alias", want, identity); err == nil {
		t.Fatal("matched symlink")
	}
	if err := os.Link(path, filepath.Join(root, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if err := MatchSecureFileInDirectoryWithIdentity(fsy, directory, "value", want, identity); !errors.Is(err, ErrUnsafeSecurePath) {
		t.Fatalf("matched multiply-linked file: %v", err)
	}
}

func TestMatchSecureFileContent(t *testing.T) {
	for _, test := range []struct {
		name, got, want string
		match           bool
	}{
		{name: "equal", got: "evidence", want: "evidence", match: true},
		{name: "empty", match: true},
		{name: "changed", got: "evidencf", want: "evidence"},
		{name: "short", got: "evidenc", want: "evidence"},
		{name: "long", got: "evidence!", want: "evidence"},
		{name: "unexpected", got: "!"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := matchSecureFileContent(bytes.NewBufferString(test.got), []byte(test.want))
			if (err == nil) != test.match || (!test.match && !errors.Is(err, ErrUnsafeSecurePath)) {
				t.Fatalf("match result = %v, want match=%t", err, test.match)
			}
		})
	}
}

func TestMatchSecureFileContentPropagatesReadFailure(t *testing.T) {
	readErr := errors.New("read failed")
	if err := matchSecureFileContent(&failingSecureMatchReader{err: readErr}, []byte("x")); !errors.Is(err, readErr) {
		t.Fatalf("hidden read failure: %v", err)
	}
	if err := matchSecureFileContent(&failingSecureMatchReader{}, []byte("x")); !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("no-progress read did not fail: %v", err)
	}
}

type failingSecureMatchReader struct{ err error }

func (r *failingSecureMatchReader) Read([]byte) (int, error) { return 0, r.err }

func TestMatchSecureFileInDirectoryRetainsIdentityFences(t *testing.T) {
	for _, stage := range []string{"equal", "wrong_identity", "after_open", "after_read"} {
		t.Run(stage, func(t *testing.T) {
			fsys := NewMemFS()
			if err := EnsureSecureDirectory(fsys, "/state"); err != nil {
				t.Fatal(err)
			}
			if err := fsys.WriteFile("/state/value", []byte("trusted"), 0o600); err != nil {
				t.Fatal(err)
			}
			info, err := fsys.Stat("/state/value")
			if err != nil {
				t.Fatal(err)
			}
			identity, _ := fsys.FileIdentity(info)
			directory, err := fsys.OpenSecureDirectory("/state")
			if err != nil {
				t.Fatal(err)
			}
			defer directory.Close()
			if stage == "wrong_identity" {
				identity.Inode++
			}
			if stage == "after_open" {
				directory = &replaceAfterOpenDirectory{SecureDirectory: directory, fsys: fsys, path: "/state"}
			}
			if stage == "after_read" {
				directory = &replaceAfterReadDirectory{SecureDirectory: directory}
			}
			err = MatchSecureFileInDirectoryWithIdentity(fsys, directory, "value", []byte("trusted"), identity)
			if stage == "equal" && err != nil {
				t.Fatal(err)
			}
			if stage != "equal" && !errors.Is(err, ErrUnsafeSecurePath) {
				t.Fatalf("replacement match error=%v", err)
			}
		})
	}
}

func TestMatchSecureFileContentAllocationBound(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 6<<20)
	allocations := testing.AllocsPerRun(1, func() {
		if err := matchSecureFileContent(bytes.NewReader(data), data); err != nil {
			t.Fatal(err)
		}
	})
	if allocations > 2 {
		t.Fatalf("stream comparison allocated %.0f objects, want <=2", allocations)
	}
}

func BenchmarkMatchSecureFileContent(b *testing.B) {
	data := bytes.Repeat([]byte("x"), 6<<20)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := matchSecureFileContent(bytes.NewReader(data), data); err != nil {
			b.Fatal(err)
		}
	}
}
