package modelregistry

import "github.com/jacobcxdev/cq/internal/fsutil"

// FileOverlayStore loads overlay models from the on-disk overlay file.
type FileOverlayStore struct {
	FS   fsutil.FileSystem
	Path string
}

func (s FileOverlayStore) Load() ([]Entry, error) {
	overlays, err := LoadOverlays(s.FS, s.Path)
	if err != nil {
		return nil, err
	}
	return overlays.Models, nil
}
