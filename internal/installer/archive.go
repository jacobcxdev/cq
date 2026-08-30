package installer

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const maxExtractedExecutableBytes int64 = 128 << 20

// StagedBinary is one verified executable in caller-owned staging.
type StagedBinary struct {
	Path          string
	Digest        string
	ArchiveDigest string
}

func extractReleaseArchive(archive []byte, archiveName, destination, executableName string) (StagedBinary, error) {
	if executableName != "cq" && executableName != "cq.exe" {
		return StagedBinary{}, fmt.Errorf("invalid CQ archive executable name")
	}
	if err := validateExtractionDestination(destination); err != nil {
		return StagedBinary{}, err
	}
	switch {
	case strings.HasSuffix(archiveName, ".zip"):
		return extractReleaseZIP(archive, destination, executableName)
	case strings.HasSuffix(archiveName, ".tar.gz"):
		return extractReleaseTarGzip(archive, destination, executableName)
	default:
		return StagedBinary{}, fmt.Errorf("unsupported CQ release archive %q", archiveName)
	}
}

func validateExtractionDestination(destination string) error {
	if destination == "" || !filepath.IsAbs(destination) {
		return fmt.Errorf("release extraction destination must be absolute")
	}
	info, err := os.Lstat(destination)
	if err != nil {
		return fmt.Errorf("inspect release extraction destination: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("release extraction destination must be a directory")
	}
	return nil
}

func extractReleaseZIP(archive []byte, destination, executableName string) (StagedBinary, error) {
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return StagedBinary{}, fmt.Errorf("open CQ release ZIP: %w", err)
	}
	var executable *zip.File
	for _, file := range reader.File {
		if err := validateArchiveEntry(file.Name, executableName); err != nil {
			return StagedBinary{}, err
		}
		if executable != nil {
			return StagedBinary{}, fmt.Errorf("CQ release archive contains duplicate executable")
		}
		if !file.Mode().IsRegular() || file.Mode()&os.ModeSymlink != 0 {
			return StagedBinary{}, fmt.Errorf("CQ release archive executable is not a regular file")
		}
		if file.UncompressedSize64 > uint64(maxExtractedExecutableBytes) {
			return StagedBinary{}, fmt.Errorf("CQ release executable exceeds size limit")
		}
		executable = file
	}
	if executable == nil {
		return StagedBinary{}, fmt.Errorf("CQ release archive does not contain %s", executableName)
	}
	file, err := executable.Open()
	if err != nil {
		return StagedBinary{}, fmt.Errorf("open CQ release executable: %w", err)
	}
	defer file.Close()
	return writeStagedExecutable(destination, executableName, file, int64(executable.UncompressedSize64))
}

func extractReleaseTarGzip(archive []byte, destination, executableName string) (staged StagedBinary, resultErr error) {
	compressed := bytes.NewReader(archive)
	gzipReader, err := gzip.NewReader(compressed)
	if err != nil {
		return StagedBinary{}, fmt.Errorf("open CQ release gzip: %w", err)
	}
	gzipReader.Multistream(false)
	var writtenPath string
	defer func() {
		if resultErr != nil && writtenPath != "" {
			_ = os.Remove(writtenPath)
		}
	}()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return StagedBinary{}, fmt.Errorf("read CQ release tar: %w", err)
		}
		if err := validateArchiveEntry(header.Name, executableName); err != nil {
			return StagedBinary{}, err
		}
		if staged.Path != "" {
			return StagedBinary{}, fmt.Errorf("CQ release archive contains duplicate executable")
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return StagedBinary{}, fmt.Errorf("CQ release archive executable is not a regular file")
		}
		if header.Size < 0 || header.Size > maxExtractedExecutableBytes {
			return StagedBinary{}, fmt.Errorf("CQ release executable exceeds size limit")
		}
		staged, err = writeStagedExecutable(destination, executableName, tarReader, header.Size)
		if err != nil {
			return StagedBinary{}, err
		}
		writtenPath = staged.Path
	}
	if staged.Path == "" {
		return StagedBinary{}, fmt.Errorf("CQ release archive does not contain %s", executableName)
	}
	if _, err := io.Copy(io.Discard, gzipReader); err != nil {
		return StagedBinary{}, fmt.Errorf("verify CQ release gzip: %w", err)
	}
	if err := gzipReader.Close(); err != nil {
		return StagedBinary{}, fmt.Errorf("close CQ release gzip: %w", err)
	}
	if compressed.Len() != 0 {
		return StagedBinary{}, fmt.Errorf("CQ release gzip has trailing content")
	}
	return staged, nil
}

func validateArchiveEntry(name, executableName string) error {
	if name == "" || path.IsAbs(name) || filepath.IsAbs(name) || filepath.VolumeName(name) != "" || strings.Contains(name, `\`) || path.Clean(name) != name || name != executableName {
		return fmt.Errorf("CQ release archive contains unexpected entry %q", name)
	}
	return nil
}

func writeStagedExecutable(destination, executableName string, source io.Reader, declaredSize int64) (staged StagedBinary, resultErr error) {
	staged.Path = filepath.Join(destination, executableName)
	file, err := os.OpenFile(staged.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return StagedBinary{}, fmt.Errorf("create staged CQ executable: %w", err)
	}
	defer func() {
		if resultErr != nil {
			_ = os.Remove(staged.Path)
		}
	}()
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(source, maxExtractedExecutableBytes+1))
	if err != nil {
		_ = file.Close()
		return StagedBinary{}, fmt.Errorf("extract CQ release executable: %w", err)
	}
	if written > maxExtractedExecutableBytes || written != declaredSize {
		_ = file.Close()
		return StagedBinary{}, fmt.Errorf("CQ release executable has invalid size")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return StagedBinary{}, fmt.Errorf("sync staged CQ executable: %w", err)
	}
	if err := file.Chmod(0o700); err != nil {
		_ = file.Close()
		return StagedBinary{}, fmt.Errorf("set staged CQ executable permissions: %w", err)
	}
	if err := file.Close(); err != nil {
		return StagedBinary{}, fmt.Errorf("close staged CQ executable: %w", err)
	}
	staged.Digest = hex.EncodeToString(hash.Sum(nil))
	return staged, nil
}
