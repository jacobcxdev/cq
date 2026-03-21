package cache

import "github.com/jacobcxdev/cq/internal/fsutil"

// Re-export for backward compatibility with existing callers.
type FileSystem = fsutil.FileSystem

// OSFileSystem delegates to the real OS.
type OSFileSystem = fsutil.OSFileSystem
