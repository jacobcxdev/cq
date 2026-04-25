package modelregistry

import (
	"encoding/json"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

// DiscoverCodexClientVersion returns the client_version recorded in an
// existing models_cache.json envelope at path. Returns an empty string when
// the file is missing, unreadable, malformed, or has no client_version set.
// Callers should supply their own fallback for that case.
func DiscoverCodexClientVersion(fsys fsutil.FileSystem, path string) string {
	data, err := fsys.ReadFile(path)
	if err != nil {
		return ""
	}
	var envelope codexCacheEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return ""
	}
	return envelope.ClientVersion
}
