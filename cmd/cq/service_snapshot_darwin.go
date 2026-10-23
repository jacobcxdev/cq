//go:build darwin

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/installer"
	"github.com/jacobcxdev/cq/internal/installstate"
)

type darwinRuntimeSnapshotPin struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func darwinRuntimeSnapshotPinName(path string) string {
	digest := sha256.Sum256([]byte(path))
	return hex.EncodeToString(digest[:]) + ".json"
}

func writeDarwinRuntimeSnapshot(ctx context.Context, artifacts installer.RuntimeArtifactStore, path string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	lock, err := artifacts.Lock()
	if err != nil {
		return err
	}
	defer lock.Close()
	digest := sha256.Sum256(data)
	pin, err := json.Marshal(darwinRuntimeSnapshotPin{Path: path, SHA256: hex.EncodeToString(digest[:])})
	if err != nil {
		return err
	}
	// Publish the retention intent first. A failed overwrite leaves a digest
	// mismatch that blocks pruning rather than losing the previous snapshot.
	if err := fsutil.SecureAtomicWrite(artifacts.FS, filepath.Join(artifacts.Roots.State, "runtime-snapshots", darwinRuntimeSnapshotPinName(path)), pin); err != nil {
		return err
	}
	return fsutil.SecureAtomicWrite(artifacts.FS, path, data)
}

func darwinRuntimeSnapshotReferences(ctx context.Context, artifacts installer.RuntimeArtifactStore, executable string) ([]string, error) {
	root := filepath.Join(artifacts.Roots.State, "runtime-snapshots")
	if err := fsutil.ValidateSecureDirectory(artifacts.FS, root); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	entries, err := artifacts.FS.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(entry.Name()) != 69 || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		pinPath := filepath.Join(root, entry.Name())
		data, err := fsutil.ReadSecureFile(artifacts.FS, pinPath, maxServiceSnapshotBytes)
		if err != nil {
			return nil, err
		}
		var pin darwinRuntimeSnapshotPin
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&pin) != nil || requireServiceSnapshotEOF(decoder) != nil || validateServiceSnapshotPath(pin.Path) != nil || entry.Name() != darwinRuntimeSnapshotPinName(pin.Path) {
			return nil, fmt.Errorf("invalid runtime snapshot pin")
		}
		data, err = fsutil.ReadSecureFile(artifacts.FS, pin.Path, maxServiceSnapshotBytes)
		if errors.Is(err, os.ErrNotExist) {
			if err := artifacts.FS.Remove(pinPath); err != nil {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(data)
		if pin.SHA256 != hex.EncodeToString(digest[:]) {
			return nil, fmt.Errorf("runtime snapshot differs from retention pin")
		}
		var snapshot persistedServiceSnapshot
		decoder = json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&snapshot) != nil || requireServiceSnapshotEOF(decoder) != nil || snapshot.SchemaVersion != serviceSnapshotSchemaVersion || snapshot.Owner != installstate.OwnerHomebrew || snapshot.Executable != executable {
			return nil, fmt.Errorf("runtime snapshot identity differs")
		}
		references, err := darwinRuntimeSnapshotPaths(snapshot.Platform)
		if err != nil {
			return nil, err
		}
		paths = append(paths, references...)
	}
	return paths, nil
}

func darwinRuntimeSnapshotPaths(snapshot servicePlatformSnapshot) ([]string, error) {
	if snapshot.Manager != "launchd" || len(snapshot.Components) != 2 {
		return nil, fmt.Errorf("invalid runtime snapshot")
	}
	var paths []string
	for index, label := range []string{proxyAgentLabel, agentLabel} {
		component := snapshot.Components[index]
		if component.ID != label {
			return nil, fmt.Errorf("invalid runtime snapshot service")
		}
		if !component.Exists {
			continue
		}
		definition, err := parseDarwinLaunchAgent(component.Definition)
		args := []string{"proxy", "start"}
		if label == agentLabel {
			args = []string{"refresh"}
		}
		if err != nil || definition.Label != label || len(definition.ProgramArguments) != len(args)+1 || !equalStrings(definition.ProgramArguments[1:], args) {
			return nil, fmt.Errorf("invalid runtime snapshot executable")
		}
		paths = append(paths, definition.ProgramArguments[0])
	}
	return paths, nil
}
