package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

// testAbsPath maps in-memory POSIX fixtures to native absolute Windows paths.
func testAbsPath(path string) string {
	if runtime.GOOS != "windows" {
		return path
	}
	root := filepath.VolumeName(os.TempDir()) + string(filepath.Separator)
	return filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(path, "/")))
}

func testJSONFixturePaths(data string) string {
	if runtime.GOOS != "windows" {
		return data
	}
	for _, root := range []string{"/work", "/home", "/cache", "/system"} {
		encoded, _ := json.Marshal(testAbsPath(root))
		data = strings.ReplaceAll(data, root, string(encoded[1:len(encoded)-1]))
	}
	return data
}

func newHarnessMemFS() *fsutil.MemFS {
	fsys := fsutil.NewMemFS()
	if runtime.GOOS == "windows" {
		if err := fsys.MkdirAll(testAbsPath("/"), 0o755); err != nil {
			panic(err)
		}
	}
	return fsys
}
