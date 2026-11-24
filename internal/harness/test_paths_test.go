package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
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
	pattern := regexp.MustCompile(`"/(?:work|home|cache|system)(?:\\.|[^"\\])*"`)
	return pattern.ReplaceAllStringFunc(data, func(raw string) string {
		var path string
		if json.Unmarshal([]byte(raw), &path) != nil {
			return raw
		}
		encoded, _ := json.Marshal(testAbsPath(path))
		return string(encoded)
	})
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
