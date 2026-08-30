package installer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/httputil"
)

const (
	maxArchiveBytes   int64 = 64 << 20
	maxChecksumsBytes int64 = 1 << 20
	releaseRepository       = "jacobcxdev/cq"
)

var releaseVersionPattern = regexp.MustCompile(`^v?([0-9]+\.[0-9]+\.[0-9]+)$`)

// Release identifies one supported CQ release archive.
type Release struct {
	Version        string
	Tag            string
	GOOS           string
	GOARCH         string
	ArchiveName    string
	ArchiveURL     string
	ChecksumsURL   string
	ExecutableName string
	client         httputil.Doer
}

// NewRelease maps a tagged CQ version and platform to fixed GitHub assets.
func NewRelease(version, goos, goarch string, client httputil.Doer) (Release, error) {
	matches := releaseVersionPattern.FindStringSubmatch(version)
	if matches == nil {
		return Release{}, fmt.Errorf("invalid CQ release version %q", version)
	}
	version = matches[1]
	extension := ".tar.gz"
	executable := "cq"
	switch goos {
	case "darwin", "linux":
	case "windows":
		extension = ".zip"
		executable = "cq.exe"
	default:
		return Release{}, fmt.Errorf("unsupported CQ release operating system %q", goos)
	}
	if goarch != "amd64" && goarch != "arm64" {
		return Release{}, fmt.Errorf("unsupported CQ release architecture %q", goarch)
	}
	tag := "v" + version
	archiveName := fmt.Sprintf("cq_%s_%s_%s%s", version, goos, goarch, extension)
	baseURL := fmt.Sprintf("https://github.com/%s/releases/download/%s/", releaseRepository, tag)
	if client == nil {
		client = newReleaseHTTPClient(version)
	}
	return Release{
		Version:        version,
		Tag:            tag,
		GOOS:           goos,
		GOARCH:         goarch,
		ArchiveName:    archiveName,
		ArchiveURL:     baseURL + archiveName,
		ChecksumsURL:   baseURL + "checksums.txt",
		ExecutableName: executable,
		client:         client,
	}, nil
}

func newReleaseHTTPClient(version string) *http.Client {
	client := httputil.NewClient(2*time.Minute, version)
	client.CheckRedirect = releaseRedirectPolicy
	return client
}

func releaseRedirectPolicy(request *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("too many release redirects")
	}
	if request.URL == nil || request.URL.Scheme != "https" || request.URL.User != nil || request.URL.Port() != "" && request.URL.Port() != "443" || !allowedReleaseHost(request.URL.Hostname()) {
		return fmt.Errorf("refused release redirect to %q", request.URL)
	}
	request.Header.Del("Authorization")
	return nil
}

func allowedReleaseHost(host string) bool {
	switch strings.ToLower(host) {
	case "github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com":
		return true
	default:
		return false
	}
}

// Download fetches, verifies, and extracts release into caller-owned destination.
func (release Release) Download(ctx context.Context, destination string) (StagedBinary, error) {
	if release.client == nil {
		return StagedBinary{}, fmt.Errorf("release HTTP client is unavailable")
	}
	checksums, err := release.fetch(ctx, release.ChecksumsURL, maxChecksumsBytes)
	if err != nil {
		return StagedBinary{}, fmt.Errorf("download CQ release checksums: %w", err)
	}
	wantArchiveDigest, err := parseReleaseChecksum(checksums, release.ArchiveName)
	if err != nil {
		return StagedBinary{}, err
	}
	archive, err := release.fetch(ctx, release.ArchiveURL, maxArchiveBytes)
	if err != nil {
		return StagedBinary{}, fmt.Errorf("download CQ release archive: %w", err)
	}
	archiveDigest := sha256.Sum256(archive)
	gotArchiveDigest := hex.EncodeToString(archiveDigest[:])
	if gotArchiveDigest != wantArchiveDigest {
		return StagedBinary{}, fmt.Errorf("CQ release archive checksum mismatch")
	}
	staged, err := extractReleaseArchive(archive, release.ArchiveName, destination, release.ExecutableName)
	if err != nil {
		return StagedBinary{}, err
	}
	staged.ArchiveDigest = gotArchiveDigest
	return staged, nil
}

func (release Release) fetch(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("create release request: %w", err)
	}
	response, err := release.client.Do(request)
	if err != nil {
		return nil, err
	}
	if response == nil || response.Body == nil {
		return nil, fmt.Errorf("release server returned an empty response")
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("release server returned %s", response.Status)
	}
	if response.ContentLength > limit {
		return nil, fmt.Errorf("release response exceeds %d bytes", limit)
	}
	body, err := httputil.ReadBodyLimit(response.Body, limit)
	if err != nil {
		return nil, err
	}
	return body, nil
}

func parseReleaseChecksum(data []byte, archiveName string) (string, error) {
	if archiveName == "" || filepath.Base(archiveName) != archiveName || strings.ContainsAny(archiveName, `/\\`) {
		return "", fmt.Errorf("invalid release archive name")
	}
	var match string
	lines := bytes.Split(data, []byte{'\n'})
	for index, line := range lines {
		if len(line) == 0 && index == len(lines)-1 {
			continue
		}
		if len(line) < 67 || !bytes.Equal(line[64:66], []byte("  ")) {
			return "", fmt.Errorf("invalid release checksum line %d", index+1)
		}
		digest := string(line[:64])
		if !isLowerHexDigest(digest) {
			return "", fmt.Errorf("invalid release checksum digest on line %d", index+1)
		}
		name := string(line[66:])
		if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, `/\\`) {
			return "", fmt.Errorf("invalid release checksum filename on line %d", index+1)
		}
		if name == archiveName {
			if match != "" {
				return "", fmt.Errorf("release checksum contains duplicate %s", archiveName)
			}
			match = digest
		}
	}
	if match == "" {
		return "", fmt.Errorf("release checksum does not contain %s", archiveName)
	}
	return match, nil
}

func isLowerHexDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}
