package installer

import (
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/installstate"
)

const cqMainPackage = "github.com/jacobcxdev/cq/cmd/cq"

// GoDestinationResolver resolves the normal Go binary destination without
// editing PATH or shell profiles.
type GoDestinationResolver struct {
	GOOS        string
	Getenv      func(string) string
	GoEnvGOPATH func(context.Context) (string, error)
	Stat        func(string) (os.FileInfo, error)
	Writable    func(string) error
}

func (resolver GoDestinationResolver) Resolve(ctx context.Context) (string, error) {
	if resolver.GOOS == "" || resolver.Getenv == nil || resolver.GoEnvGOPATH == nil || resolver.Stat == nil || resolver.Writable == nil {
		return "", fmt.Errorf("invalid Go destination resolver")
	}
	directory := resolver.Getenv("GOBIN")
	if directory != "" {
		if !cleanAbsoluteTargetPath(directory, resolver.GOOS) {
			return "", fmt.Errorf("GOBIN must be a clean absolute path")
		}
	} else {
		gopath := resolver.Getenv("GOPATH")
		if gopath == "" {
			var err error
			gopath, err = resolver.GoEnvGOPATH(ctx)
			if err != nil {
				return "", fmt.Errorf("resolve go env GOPATH: %w", err)
			}
			gopath = strings.TrimSpace(gopath)
		}
		for _, entry := range splitTargetPathList(gopath, resolver.GOOS) {
			if cleanAbsoluteTargetPath(entry, resolver.GOOS) {
				directory = joinTargetPath(entry, "bin", resolver.GOOS)
				break
			}
		}
		if directory == "" {
			return "", fmt.Errorf("GOPATH has no clean absolute entry")
		}
	}
	info, err := resolver.Stat(directory)
	if err != nil {
		return "", fmt.Errorf("inspect Go binary directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("Go binary destination is not a directory")
	}
	if err := resolver.Writable(directory); err != nil {
		return "", fmt.Errorf("Go binary destination is not writable: %w", err)
	}
	if !pathContainsDirectory(resolver.Getenv("PATH"), directory, resolver.GOOS) {
		return "", fmt.Errorf("Go binary destination %s is absent from PATH", directory)
	}
	return joinTargetPath(directory, goDestinationExecutableName(resolver.GOOS), resolver.GOOS), nil
}

func goDestinationExecutableName(goos string) string {
	if goos == "windows" {
		return "cq.exe"
	}
	return "cq"
}

func splitTargetPathList(value, goos string) []string {
	separator := ":"
	if goos == "windows" {
		separator = ";"
	}
	return strings.Split(value, separator)
}

func pathContainsDirectory(pathValue, directory, goos string) bool {
	for _, entry := range splitTargetPathList(pathValue, goos) {
		if goos == "windows" {
			if equalWindowsDirectory(entry, directory) {
				return true
			}
		} else if entry == directory {
			return true
		}
	}
	return false
}

func equalWindowsDirectory(left, right string) bool {
	normalise := func(value string) string {
		value = strings.ReplaceAll(value, "/", `\`)
		if len(value) > 3 {
			value = strings.TrimRight(value, `\`)
		}
		return value
	}
	return strings.EqualFold(normalise(left), normalise(right))
}

func cleanAbsoluteTargetPath(value, goos string) bool {
	if value == "" {
		return false
	}
	if goos == runtime.GOOS {
		return filepath.IsAbs(value) && filepath.Clean(value) == value
	}
	if goos != "windows" {
		return strings.HasPrefix(value, "/") && cleanSlashPath(value)
	}
	normalised := strings.ReplaceAll(value, "/", `\`)
	if len(normalised) < 3 || normalised[1] != ':' || normalised[2] != '\\' || !isASCIILetter(normalised[0]) {
		return false
	}
	parts := strings.Split(normalised[3:], `\`)
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func cleanSlashPath(value string) bool {
	parts := strings.Split(strings.TrimPrefix(value, "/"), "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func isASCIILetter(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z'
}

func joinTargetPath(directory, name, goos string) string {
	if goos == runtime.GOOS {
		return filepath.Join(directory, name)
	}
	if goos == "windows" {
		return strings.TrimRight(strings.ReplaceAll(directory, "/", `\`), `\`) + `\` + name
	}
	return strings.TrimRight(directory, "/") + "/" + name
}

// BinaryOwnership describes whether an exact destination may be installed.
type BinaryOwnership uint8

const (
	BinaryAbsent BinaryOwnership = iota + 1
	BinaryOwned
	BinaryAdoptable
	BinaryForeign
)

// BinaryClassifier classifies an exact executable using ownership state and Go
// build metadata.
type BinaryClassifier struct {
	FS            fsutil.FileSystem
	State         installationState
	ReadBuildInfo func(string) (*buildinfo.BuildInfo, error)
}

func (classifier BinaryClassifier) Classify(owner installstate.Owner, executable string) (BinaryOwnership, error) {
	if classifier.FS == nil || classifier.State == nil || !owner.Valid() || !filepath.IsAbs(executable) || filepath.Clean(executable) != executable {
		return 0, fmt.Errorf("invalid binary ownership classifier")
	}
	record, err := classifier.State.Load()
	if err == nil {
		if record.Owner == owner && record.Executable == executable {
			return BinaryOwned, nil
		}
		return 0, fmt.Errorf(
			"%w: existing owner %q executable %q; requested owner %q executable %q",
			installstate.ErrOwnershipConflict,
			record.Owner,
			record.Executable,
			owner,
			executable,
		)
	}
	if !errors.Is(err, installstate.ErrNotInstalled) {
		return 0, err
	}
	if inspector, ok := classifier.FS.(fsutil.SecurePathInspector); ok {
		info, err := inspector.Lstat(executable)
		if errors.Is(err, os.ErrNotExist) {
			return BinaryAbsent, nil
		}
		if err != nil {
			return 0, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return BinaryForeign, nil
		}
	} else {
		info, err := classifier.FS.Stat(executable)
		if errors.Is(err, os.ErrNotExist) {
			return BinaryAbsent, nil
		}
		if err != nil {
			return 0, err
		}
		if !info.Mode().IsRegular() {
			return BinaryForeign, nil
		}
	}
	readBuildInfo := classifier.ReadBuildInfo
	if readBuildInfo == nil {
		readBuildInfo = buildinfo.ReadFile
	}
	info, err := readBuildInfo(executable)
	if err != nil || info == nil || info.Path != cqMainPackage {
		return BinaryForeign, nil
	}
	return BinaryAdoptable, nil
}
