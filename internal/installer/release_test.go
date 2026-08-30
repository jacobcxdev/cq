package installer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type releaseDoerFunc func(*http.Request) (*http.Response, error)

func (do releaseDoerFunc) Do(request *http.Request) (*http.Response, error) {
	return do(request)
}

func TestReleaseMapsSupportedPlatforms(t *testing.T) {
	tests := []struct {
		goos       string
		goarch     string
		extension  string
		executable string
	}{
		{goos: "darwin", goarch: "amd64", extension: ".tar.gz", executable: "cq"},
		{goos: "darwin", goarch: "arm64", extension: ".tar.gz", executable: "cq"},
		{goos: "linux", goarch: "amd64", extension: ".tar.gz", executable: "cq"},
		{goos: "linux", goarch: "arm64", extension: ".tar.gz", executable: "cq"},
		{goos: "windows", goarch: "amd64", extension: ".zip", executable: "cq.exe"},
		{goos: "windows", goarch: "arm64", extension: ".zip", executable: "cq.exe"},
	}
	for _, test := range tests {
		t.Run(test.goos+"_"+test.goarch, func(t *testing.T) {
			release, err := NewRelease("v0.27.0", test.goos, test.goarch, releaseDoerFunc(nil))
			if err != nil {
				t.Fatal(err)
			}
			wantArchive := "cq_0.27.0_" + test.goos + "_" + test.goarch + test.extension
			if release.Version != "0.27.0" || release.Tag != "v0.27.0" || release.ArchiveName != wantArchive || release.ExecutableName != test.executable {
				t.Fatalf("release = %#v", release)
			}
			wantBase := "https://github.com/jacobcxdev/cq/releases/download/v0.27.0/"
			if release.ArchiveURL != wantBase+wantArchive || release.ChecksumsURL != wantBase+"checksums.txt" {
				t.Fatalf("release URLs = %q / %q", release.ArchiveURL, release.ChecksumsURL)
			}
		})
	}
}

func TestReleaseRejectsUnsupportedPlatformOrVersion(t *testing.T) {
	for _, test := range []struct {
		version string
		goos    string
		goarch  string
	}{
		{version: "0.27.0", goos: "freebsd", goarch: "amd64"},
		{version: "0.27.0", goos: "linux", goarch: "386"},
		{version: "dev", goos: "linux", goarch: "amd64"},
		{version: "0.27.0/evil", goos: "linux", goarch: "amd64"},
	} {
		if _, err := NewRelease(test.version, test.goos, test.goarch, releaseDoerFunc(nil)); err == nil {
			t.Fatalf("NewRelease(%q, %q, %q) succeeded", test.version, test.goos, test.goarch)
		}
	}
}

func TestReleaseRedirectPolicyAllowsOnlyReleaseHosts(t *testing.T) {
	via := []*http.Request{{URL: mustReleaseURL(t, "https://github.com/jacobcxdev/cq/releases/download/v0.27.0/cq.zip")}}
	for _, host := range []string{"github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com"} {
		request := &http.Request{URL: mustReleaseURL(t, "https://"+host+"/asset")}
		request.Header = make(http.Header)
		request.Header.Set("Authorization", "secret")
		if err := releaseRedirectPolicy(request, via); err != nil {
			t.Fatalf("redirect to %s: %v", host, err)
		}
		if request.Header.Get("Authorization") != "" {
			t.Fatalf("redirect to %s retained authorization", host)
		}
	}
	for _, rawURL := range []string{"https://example.com/asset", "http://github.com/asset", "https://github.com:444/asset", "https://user@github.com/asset"} {
		if err := releaseRedirectPolicy(&http.Request{URL: mustReleaseURL(t, rawURL), Header: make(http.Header)}, via); err == nil {
			t.Fatalf("redirect to %s succeeded", rawURL)
		}
	}
}

func TestReleaseDownloadVerifiesAndExtractsArchive(t *testing.T) {
	archive := makeZIPArchive(t, []archiveTestEntry{{name: "cq.exe", body: []byte("cq-binary"), mode: 0o700}})
	archiveDigest := sha256.Sum256(archive)
	checksums := fmt.Sprintf("%x  cq_0.27.0_windows_amd64.zip\n", archiveDigest)
	release, err := NewRelease("0.27.0", "windows", "amd64", releaseFixtureDoer(checksums, archive, http.StatusOK, http.StatusOK))
	if err != nil {
		t.Fatal(err)
	}
	staged, err := release.Download(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(staged.Path) != "cq.exe" {
		t.Fatalf("staged path = %q", staged.Path)
	}
	body, err := os.ReadFile(staged.Path)
	if err != nil {
		t.Fatal(err)
	}
	wantDigest := sha256.Sum256(body)
	if string(body) != "cq-binary" || staged.Digest != fmt.Sprintf("%x", wantDigest) || staged.ArchiveDigest != fmt.Sprintf("%x", archiveDigest) {
		t.Fatalf("staged = %#v; body = %q", staged, body)
	}
}

func TestReleaseDownloadRejectsHTTPFailuresAndOversizedBodies(t *testing.T) {
	tests := []struct {
		name           string
		checksumStatus int
		archiveStatus  int
		checksumLength int64
		archiveLength  int64
	}{
		{name: "checksums status", checksumStatus: http.StatusNotFound, archiveStatus: http.StatusOK},
		{name: "archive status", checksumStatus: http.StatusOK, archiveStatus: http.StatusBadGateway},
		{name: "checksums too large", checksumStatus: http.StatusOK, archiveStatus: http.StatusOK, checksumLength: maxChecksumsBytes + 1},
		{name: "archive too large", checksumStatus: http.StatusOK, archiveStatus: http.StatusOK, archiveLength: maxArchiveBytes + 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			archive := makeZIPArchive(t, []archiveTestEntry{{name: "cq.exe", body: []byte("cq"), mode: 0o700}})
			digest := sha256.Sum256(archive)
			checksums := fmt.Sprintf("%x  cq_0.27.0_windows_amd64.zip\n", digest)
			doer := releaseFixtureDoer(checksums, archive, test.checksumStatus, test.archiveStatus)
			doer = withReleaseContentLength(doer, test.checksumLength, test.archiveLength)
			release, err := NewRelease("0.27.0", "windows", "amd64", doer)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := release.Download(context.Background(), t.TempDir()); err == nil {
				t.Fatal("Download succeeded")
			}
		})
	}
}

func TestReleaseDownloadRejectsChecksumMismatchAndMalformedManifest(t *testing.T) {
	archive := makeZIPArchive(t, []archiveTestEntry{{name: "cq.exe", body: []byte("cq"), mode: 0o700}})
	for _, checksums := range []string{
		strings.Repeat("0", 64) + "  cq_0.27.0_windows_amd64.zip\n",
		strings.Repeat("A", 64) + "  cq_0.27.0_windows_amd64.zip\n",
		strings.Repeat("0", 64) + " cq_0.27.0_windows_amd64.zip\n",
		strings.Repeat("0", 64) + "  ../cq_0.27.0_windows_amd64.zip\n",
		strings.Repeat("0", 64) + "  other.zip\n",
	} {
		release, err := NewRelease("0.27.0", "windows", "amd64", releaseFixtureDoer(checksums, archive, http.StatusOK, http.StatusOK))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := release.Download(context.Background(), t.TempDir()); err == nil {
			t.Fatalf("Download accepted checksums %q", checksums)
		}
	}
}

func TestParseReleaseChecksumRejectsDuplicateMatch(t *testing.T) {
	line := strings.Repeat("0", 64) + "  cq.zip\n"
	if _, err := parseReleaseChecksum([]byte(line+line), "cq.zip"); err == nil {
		t.Fatal("duplicate checksum accepted")
	}
}

func releaseFixtureDoer(checksums string, archive []byte, checksumStatus, archiveStatus int) releaseDoerFunc {
	return func(request *http.Request) (*http.Response, error) {
		status := checksumStatus
		body := []byte(checksums)
		if strings.HasSuffix(request.URL.Path, ".zip") || strings.HasSuffix(request.URL.Path, ".tar.gz") {
			status = archiveStatus
			body = archive
		}
		return &http.Response{
			StatusCode:    status,
			Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
			Body:          io.NopCloser(bytes.NewReader(body)),
			ContentLength: int64(len(body)),
			Header:        make(http.Header),
			Request:       request,
		}, nil
	}
}

func withReleaseContentLength(doer releaseDoerFunc, checksums, archive int64) releaseDoerFunc {
	return func(request *http.Request) (*http.Response, error) {
		response, err := doer(request)
		if err != nil {
			return nil, err
		}
		if strings.HasSuffix(request.URL.Path, ".zip") || strings.HasSuffix(request.URL.Path, ".tar.gz") {
			if archive > 0 {
				response.ContentLength = archive
			}
		} else if checksums > 0 {
			response.ContentLength = checksums
		}
		return response, nil
	}
}

func mustReleaseURL(t *testing.T, rawURL string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
