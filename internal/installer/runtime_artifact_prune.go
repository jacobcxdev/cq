package installer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

// Lock serialises staging, transaction preparation and pruning. Transaction
// preparation holds it until both executable references are durable.
func (store RuntimeArtifactStore) Lock() (fsutil.ExclusiveLock, error) {
	if store.FS == nil || !filepath.IsAbs(store.Roots.State) || filepath.Clean(store.Roots.State) != store.Roots.State {
		return nil, ErrRuntimeArtifact
	}
	if err := fsutil.EnsureSecureDirectory(store.FS, store.Roots.State); err != nil {
		return nil, err
	}
	locker, ok := store.FS.(fsutil.ExclusiveLocker)
	if !ok {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	return locker.OpenExclusiveLock(filepath.Join(store.Roots.State, "runtime-artifacts.lock"), 0o600)
}

// Prune reads durable references while transaction preparation is excluded.
// It only removes recognised executables; unknown entries remain untouched.
func (store RuntimeArtifactStore) Prune(ctx context.Context, references func() ([]string, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	lock, err := store.Lock()
	if err != nil {
		return err
	}
	defer lock.Close()
	paths, err := references()
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return ErrRuntimeArtifact
	}
	keep := make(map[string]bool, len(paths))
	for _, path := range paths {
		digest := filepath.Base(filepath.Dir(path))
		if !runtimeArtifactDigest(digest) || path != store.artifactPath(digest) {
			return ErrRuntimeArtifact
		}
		keep[digest] = true
	}
	root := filepath.Join(store.Roots.State, "runtime-artifacts")
	if err := fsutil.ValidateSecureDirectory(store.FS, root); err != nil {
		return err
	}
	opener, ok := store.FS.(fsutil.SecureDirectoryOpener)
	if !ok {
		return fsutil.ErrSecureCapabilityUnavailable
	}
	directory, err := opener.OpenSecureDirectory(root)
	if err != nil {
		return err
	}
	defer directory.Close()
	parent, ok := directory.(fsutil.DurableDirectory)
	if !ok {
		return fsutil.ErrSecureCapabilityUnavailable
	}
	reader, ok := directory.(fsutil.SecureDirectoryReader)
	if !ok {
		return fsutil.ErrSecureCapabilityUnavailable
	}
	entries, err := reader.ReadDir()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if keep[entry.Name()] || !entry.IsDir() || !runtimeArtifactDigest(entry.Name()) {
			continue
		}
		child, err := parent.OpenDirectory(entry.Name())
		if err != nil {
			return err
		}
		removed, pruneErr := store.pruneRuntimeArtifact(ctx, child, entry.Name())
		closeErr := child.Close()
		if err := errors.Join(pruneErr, closeErr); err != nil {
			return err
		}
		if removed {
			// Remove only an empty directory, never recursively traverse a path.
			if err := store.FS.Remove(filepath.Join(root, entry.Name())); err != nil {
				return err
			}
			if err := directory.Sync(); err != nil {
				return err
			}
		}
	}
	return nil
}

func runtimeArtifactDigest(digest string) bool {
	decoded, err := hex.DecodeString(digest)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == digest
}

func (store RuntimeArtifactStore) pruneRuntimeArtifact(ctx context.Context, child fsutil.DurableDirectory, digest string) (bool, error) {
	inspector, ok := store.FS.(fsutil.SecurePathInspector)
	if !ok {
		return false, fsutil.ErrSecureCapabilityUnavailable
	}
	info, err := child.Stat()
	if err != nil {
		return false, err
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || fsutil.ValidateSecureOwner(inspector, info) != nil {
		return false, nil
	}
	directory, ok := child.(fsutil.SecureDirectory)
	if !ok {
		return false, fsutil.ErrSecureCapabilityUnavailable
	}
	reader, ok := child.(fsutil.SecureDirectoryReader)
	if !ok {
		return false, fsutil.ErrSecureCapabilityUnavailable
	}
	entries, err := reader.ReadDir()
	if err != nil {
		return false, err
	}
	if len(entries) == 0 {
		return true, nil // Finish a previous prune interrupted after unlink.
	}
	if len(entries) != 1 || entries[0].Name() != "cq" || !entries[0].Type().IsRegular() {
		return false, nil
	}
	file, err := directory.OpenNoFollow("cq")
	if err != nil {
		return false, err
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		return false, err
	}
	identity, ok := inspector.FileIdentity(info)
	if !ok || identity.Links != 1 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o500 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || fsutil.ValidateSecureOwner(inspector, info) != nil {
		return false, nil
	}
	data, err := io.ReadAll(io.LimitReader(file, maxRuntimeArtifactBytes+1))
	if err != nil {
		return false, err
	}
	if len(data) > maxRuntimeArtifactBytes || validateRuntimeBuild(data) != nil {
		return false, nil
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != digest {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	remover, ok := directory.(fsutil.IdentityBoundRemover)
	if !ok {
		return false, fsutil.ErrSecureCapabilityUnavailable
	}
	if err := remover.RemoveChecked("cq", identity); err != nil {
		return false, err
	}
	return true, directory.Sync()
}
