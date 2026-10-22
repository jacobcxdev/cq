package dots

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
)

// prepareNativeAppcast runs only after Start has established that the app is
// stopped. The main process reads this flag before the renderer's Statsig gates,
// so the bootstrap override alone cannot fix the first launch's cached feed.
func prepareNativeAppcast(path string) error {
	if err := launcherOwned(filepath.Dir(path), true); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	file, err := (fsutil.OSFileSystem{}).OpenNoFollow(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil // A fresh profile defaults to the packaged vendor feed.
	}
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || !owned(info) || info.Mode().Perm()&0o022 != 0 || info.Size() > 1<<20 {
		return errors.New("Native updater state must be an owned, bounded regular file without shared writes")
	}
	data, err := httputil.ReadBody(file)
	if err != nil {
		return err
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(data, &state); err != nil || state == nil {
		return errors.New("Native updater state must be a JSON object")
	}
	value, ok := state["backendAppcastEnabled"]
	if !ok {
		return nil
	}
	switch strings.TrimSpace(string(value)) {
	case "false":
		return nil
	case "true":
	default:
		return errors.New("Native updater backend flag must be a boolean")
	}
	state["backendAppcastEnabled"] = json.RawMessage("false")
	data, err = json.Marshal(state)
	if err != nil {
		return err
	}
	return launcherWrite(path, append(data, '\n'), info.Mode().Perm())
}
