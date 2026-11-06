package dots

import (
	"errors"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Rspack emits numeric chunk identifiers and a content hash. Restrict discovery
// to the renderer assets directory and its immediate async subdirectory.
var rspackAssetPattern = regexp.MustCompile(`^(?:async/)?[0-9]+\.[a-f0-9]{1,64}\.js$`)

func readNativeRspackModules(f *os.File, assets map[string]nativeArchiveEntry, dataStart, archiveSize int64) (map[string]string, error) {
	entries := make(map[string]nativeArchiveEntry)
	for name, entry := range assets {
		if rspackAssetPattern.MatchString(name) {
			entries[name] = entry
		}
	}
	async := assets["async"]
	if async.Unpacked || async.Link != "" {
		return nil, errors.New("renderer async directory invalid")
	}
	for name, entry := range async.Files {
		if rspackAssetPattern.MatchString("async/" + name) {
			entries["async/"+name] = entry
		}
	}
	if len(entries) == 0 || len(entries) > 12000 {
		return nil, errors.New("native renderer modules unavailable or excessive")
	}
	modules := make(map[string]string)
	var total int64
	for name, entry := range entries {
		total += entry.Size
		if entry.Unpacked || entry.Link != "" || entry.Size < 1 || entry.Size > 32<<20 || total > 768<<20 {
			return nil, errors.New("renderer module bounds invalid")
		}
		offset, err := strconv.ParseInt(entry.Offset, 10, 64)
		if err != nil || offset < 0 || offset > archiveSize-dataStart || entry.Size > archiveSize-dataStart-offset {
			return nil, errors.New("renderer module offset invalid")
		}
		data := make([]byte, entry.Size)
		if _, err := f.ReadAt(data, dataStart+offset); err != nil {
			return nil, errors.New("renderer module unavailable")
		}
		source := string(data)
		for _, needle := range []string{"__webpack_require__", "authenticatedAccountId", "chatGptAuthenticatedAccountId$", "tppLocalExecutor", ".cache?.accountId", "cloud-aeon-profiles-v1:", "dispatchMessage(", "Persisted atom store accessed before initialization", "codex:persisted-atom:", "getAdditionalHeaders:", "getRequestSignal:"} {
			if strings.Contains(source, needle) {
				modules[name] = source
				break
			}
		}
	}
	return modules, nil
}
