// Command nativepackage validates CQ's signed macOS component packages.
package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const repository = "jacobcxdev/cq"

var (
	versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	commitPattern  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	digestPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type manifest struct {
	SchemaVersion int             `json:"schema_version"`
	Version       string          `json:"version"`
	SourceCommit  string          `json:"source_commit"`
	AssetID       int64           `json:"asset_id"`
	SHA256        string          `json:"sha256"`
	MinimumMacOS  string          `json:"minimum_macos"`
	Bundles       *installBundles `json:"bundles,omitempty"`
}

type installArchive struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

type installBundles struct {
	ARM64 installArchive `json:"arm64"`
	AMD64 installArchive `json:"amd64"`
}

func (bundles *installBundles) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if len(fields) != 2 || fields["arm64"] == nil || fields["amd64"] == nil {
		return errors.New("install bundles must contain exactly arm64 and amd64")
	}
	type wireBundles installBundles
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode((*wireBundles)(bundles))
}

func (m manifest) stagingTag() string { return "cq-native-stage-v" + m.Version }

func (m manifest) tag() string {
	if m.SchemaVersion == 2 {
		return "v" + m.Version
	}
	return "cq-network-v" + m.Version
}
func (m manifest) asset() string { return "CQNetwork_" + m.Version + "_universal.zip" }
func (m manifest) url() string {
	return "https://github.com/" + repository + "/releases/download/" + m.tag() + "/" + m.asset()
}
func (m manifest) validate() error {
	if (m.SchemaVersion != 1 && m.SchemaVersion != 2) || !versionPattern.MatchString(m.Version) || !commitPattern.MatchString(m.SourceCommit) || m.AssetID <= 0 || !digestPattern.MatchString(m.SHA256) || m.MinimumMacOS != "13.0" {
		return errors.New("native release manifest has invalid or missing identity, source, checksum or minimum macOS")
	}
	if m.Bundles != nil {
		if m.SchemaVersion != 2 {
			return errors.New("bundled installation requires a parent release pin")
		}
		for arch, archive := range map[string]installArchive{"arm64": m.Bundles.ARM64, "amd64": m.Bundles.AMD64} {
			if archive.URL != m.bundleURL(arch) || !digestPattern.MatchString(archive.SHA256) {
				return errors.New("install bundle URL or SHA-256 differs from the parent release")
			}
		}
	}
	return nil
}

func (m manifest) bundleName(arch string) string {
	return "cq_" + m.Version + "_darwin_" + arch + "_bundle.tar.gz"
}
func (m manifest) bundleURL(arch string) string {
	return "https://github.com/" + repository + "/releases/download/" + m.tag() + "/" + m.bundleName(arch)
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "nativepackage:", err)
		os.Exit(1)
	}
}

func run(args []string, output io.Writer) error {
	if len(args) == 6 && (args[0] == "pin" || args[0] == "pin-parent") {
		id, err := strconv.ParseInt(args[3], 10, 64)
		if err != nil {
			return errors.New("invalid native release asset ID")
		}
		m := manifest{SchemaVersion: 1, Version: args[1], SourceCommit: args[2], AssetID: id, SHA256: args[4], MinimumMacOS: args[5]}
		if args[0] == "pin-parent" {
			m.SchemaVersion = 2
		}
		if err := m.validate(); err != nil {
			return err
		}
		encoder := json.NewEncoder(output)
		encoder.SetIndent("", "  ")
		return encoder.Encode(m)
	}
	if len(args) < 2 {
		return errors.New("usage: nativepackage info MANIFEST | verify MANIFEST ZIP RELEASE_JSON [--allow-draft] | verify-install MANIFEST TAR RELEASE_JSON [--allow-draft] | verify-source MANIFEST COMMIT | published MANIFEST RELEASE_JSON | cask MANIFEST OUTPUT [--bundles BUNDLES_JSON] | pin[-parent] VERSION SOURCE_COMMIT ASSET_ID SHA256 MINIMUM_MACOS")
	}
	var m manifest
	if err := decodeFile(args[1], &m, 16<<10); err != nil {
		return err
	}
	if err := m.validate(); err != nil {
		return err
	}
	switch {
	case args[0] == "info" && len(args) == 2:
		info := map[string]any{"schema_version": m.SchemaVersion, "tag": m.tag(), "asset": m.asset(), "asset_id": m.AssetID, "url": m.url(), "version": m.Version, "source_commit": m.SourceCommit, "minimum_macos": m.MinimumMacOS, "install_url": m.url(), "install_sha256": m.SHA256}
		if m.SchemaVersion == 2 {
			info["staging_tag"] = m.stagingTag()
		}
		if m.Bundles != nil {
			var archive installArchive
			switch runtime.GOARCH {
			case "arm64":
				archive = m.Bundles.ARM64
			case "amd64":
				archive = m.Bundles.AMD64
			default:
				return errors.New("unsupported install bundle architecture")
			}
			info["install_url"], info["install_sha256"] = archive.URL, archive.SHA256
		}
		return json.NewEncoder(output).Encode(info)
	case args[0] == "verify-source" && len(args) == 3:
		return verifySource(m, args[2])
	case args[0] == "verify" && (len(args) == 4 || len(args) == 5 && args[4] == "--allow-draft"):
		if err := verifyRelease(m, args[3], len(args) == 5); err != nil {
			return err
		}
		return verifyArchive(m, args[2])
	case args[0] == "verify-install" && (len(args) == 4 || len(args) == 5 && args[4] == "--allow-draft"):
		if m.Bundles == nil {
			return errors.New("install bundles are missing")
		}
		if err := verifyRelease(m, args[3], len(args) == 5); err != nil {
			return err
		}
		archive := m.Bundles.ARM64
		if runtime.GOARCH == "amd64" {
			archive = m.Bundles.AMD64
		} else if runtime.GOARCH != "arm64" {
			return errors.New("unsupported install bundle architecture")
		}
		return verifyInstallArchive(archive.SHA256, args[2])
	case args[0] == "published" && len(args) == 3:
		return verifyRelease(m, args[2], false)
	case args[0] == "cask" && (len(args) == 3 || len(args) == 5 && args[3] == "--bundles"):
		if len(args) == 5 {
			if err := decodeFile(args[4], &m.Bundles, 16<<10); err != nil {
				return err
			}
			if m.Bundles == nil {
				return errors.New("install bundles are missing")
			}
			if err := m.validate(); err != nil {
				return err
			}
		}
		return os.WriteFile(args[2], []byte(renderCask(m)), 0o644)
	default:
		return errors.New("invalid nativepackage arguments")
	}
}

func verifySource(m manifest, commit string) error {
	if !commitPattern.MatchString(commit) {
		return errors.New("native release target must be a full commit SHA")
	}
	git := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		output, err := exec.CommandContext(ctx, "git", args...).Output()
		return strings.TrimSpace(string(output)), err
	}
	if _, err := git("merge-base", "--is-ancestor", m.SourceCommit, commit); err != nil {
		return errors.New("native build source is not a proven ancestor of release source")
	}
	sourceTree, err := git("rev-parse", "--verify", m.SourceCommit+":native/macos-network")
	if err != nil || !commitPattern.MatchString(sourceTree) {
		return errors.New("native build source tree is unavailable")
	}
	releaseTree, err := git("rev-parse", "--verify", commit+":native/macos-network")
	if err != nil || sourceTree != releaseTree {
		return errors.New("native release source tree differs from signed build")
	}
	return nil
}

func decodeFile(file string, value any, limit int64) error {
	info, err := os.Stat(file)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return errors.New("native release metadata is not a bounded regular file")
	}
	reader, err := os.Open(file)
	if err != nil {
		return err
	}
	defer reader.Close()
	decoder := json.NewDecoder(io.LimitReader(reader, limit+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("native release metadata has trailing data")
	}
	return nil
}

func verifyRelease(m manifest, file string, allowDraft bool) error {
	// GitHub release responses contain additional fields. Decode those while
	// keeping the reviewed manifest itself closed above.
	info, err := os.Stat(file)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return errors.New("native release API response exceeds limit")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var release struct {
		Tag        string `json:"tag_name"`
		Target     string `json:"target_commitish"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		Assets     []struct {
			ID     int64  `json:"id"`
			Name   string `json:"name"`
			Digest string `json:"digest"`
			State  string `json:"state"`
			URL    string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(data, &release); err != nil {
		return err
	}
	staging := m.SchemaVersion == 2 && release.Tag == m.stagingTag() && release.Draft && allowDraft && release.Target == m.SourceCommit
	if (release.Tag != m.tag() && !staging) || release.Draft && !allowDraft || release.Prerelease != (m.SchemaVersion == 1) {
		return errors.New("native release tag, source commit or publication state differs from reviewed manifest")
	}
	if m.SchemaVersion == 1 {
		if release.Target != m.SourceCommit {
			return errors.New("native release source commit differs from reviewed manifest")
		}
	} else if err := verifySource(m, release.Target); err != nil {
		return err
	}
	if m.SchemaVersion == 2 && !release.Draft {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		commit, err := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "refs/tags/"+m.tag()+"^{commit}").Output()
		if err != nil || strings.TrimSpace(string(commit)) != release.Target {
			return errors.New("parent release tag does not identify its declared source commit")
		}
	}
	count := 0
	for _, asset := range release.Assets {
		if asset.Name != m.asset() {
			continue
		}
		count++
		validURL := asset.URL == m.url()
		if release.Draft && allowDraft {
			// GitHub assigns draft downloads an untagged URL, then changes it
			// on publication. Downloads use the separately pinned asset ID.
			validURL = strings.HasPrefix(asset.URL, "https://github.com/"+repository+"/releases/download/") && strings.HasSuffix(asset.URL, "/"+m.asset())
		}
		if asset.ID != m.AssetID || asset.Digest != "sha256:"+m.SHA256 || asset.State != "uploaded" || !validURL {
			return errors.New("native release asset identity or digest differs from reviewed manifest")
		}
	}
	if count != 1 {
		return errors.New("native release asset is absent or ambiguous")
	}
	if m.Bundles != nil {
		for arch, bundle := range map[string]installArchive{"arm64": m.Bundles.ARM64, "amd64": m.Bundles.AMD64} {
			count := 0
			for _, asset := range release.Assets {
				if asset.Name != m.bundleName(arch) {
					continue
				}
				count++
				validURL := asset.URL == bundle.URL
				if release.Draft && allowDraft {
					validURL = strings.HasPrefix(asset.URL, "https://github.com/"+repository+"/releases/download/") && strings.HasSuffix(asset.URL, "/"+m.bundleName(arch))
				}
				if asset.ID <= 0 || asset.Digest != "sha256:"+bundle.SHA256 || asset.State != "uploaded" || !validURL {
					return errors.New("install release asset identity or digest differs from rendered pin")
				}
			}
			if count != 1 {
				return errors.New("install release asset is absent or ambiguous")
			}
		}
	}
	return nil
}

func verifyArchiveDigest(digest, file string) error {
	info, err := os.Stat(file)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 512<<20 {
		return errors.New("native archive is not a bounded regular file")
	}
	reader, err := os.Open(file)
	if err != nil {
		return err
	}
	defer reader.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(reader, (512<<20)+1)); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		return errors.New("native archive SHA-256 differs from reviewed manifest")
	}
	return nil
}

func verifyArchive(m manifest, file string) error {
	if err := verifyArchiveDigest(m.SHA256, file); err != nil {
		return err
	}
	archive, err := zip.OpenReader(file)
	if err != nil {
		return err
	}
	defer archive.Close()
	if len(archive.File) > 10000 {
		return errors.New("native archive has too many entries")
	}
	var size uint64
	seen := make(map[string]bool)
	symlinks := make(map[string]bool)
	for _, entry := range archive.File {
		name := strings.TrimSuffix(entry.Name, "/")
		if name == "" || strings.Contains(name, "\\") || path.IsAbs(name) || path.Clean(name) != name || (name != "CQNetwork.app" && !strings.HasPrefix(name, "CQNetwork.app/") && name != "__MACOSX" && !strings.HasPrefix(name, "__MACOSX/")) || seen[name] {
			return errors.New("native archive has an unsafe or duplicate entry")
		}
		seen[name] = true
		size += entry.UncompressedSize64
		if size > 512<<20 || entry.UncompressedSize64 > 256<<20 {
			return errors.New("native archive expansion exceeds size limit")
		}
		if entry.Mode()&os.ModeSymlink != 0 {
			symlinks[name] = true
			if entry.UncompressedSize64 > 1024 {
				return errors.New("native archive symlink exceeds size limit")
			}
			link, err := entry.Open()
			if err != nil {
				return err
			}
			var target bytes.Buffer
			_, copyErr := io.Copy(&target, io.LimitReader(link, 1025))
			closeErr := link.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			resolved := path.Clean(path.Join(path.Dir(name), target.String()))
			if target.Len() > 1024 || path.IsAbs(target.String()) || strings.Contains(target.String(), "\\") || !strings.HasPrefix(resolved, "CQNetwork.app/") {
				return errors.New("native archive symlink escapes the application")
			}
		} else if !entry.Mode().IsRegular() && !entry.Mode().IsDir() {
			return errors.New("native archive has unsupported file type")
		}
	}
	// Check after reading every entry so ZIP ordering cannot conceal a symlink
	// ancestor. Lexically safe link targets may compose to escape the bundle.
	for name := range seen {
		for ancestor := path.Dir(name); ancestor != "."; ancestor = path.Dir(ancestor) {
			if symlinks[ancestor] {
				return errors.New("native archive entry has a symlink ancestor")
			}
		}
	}
	for _, name := range []string{"CQNetwork.app/Contents/Info.plist", "CQNetwork.app/Contents/MacOS/CQNetwork", "CQNetwork.app/Contents/Library/SystemExtensions/dev.jacobcx.cq.network.proxy.systemextension/Contents/Info.plist", "CQNetwork.app/Contents/Library/SystemExtensions/dev.jacobcx.cq.network.proxy.systemextension/Contents/MacOS/CQNetworkProxy"} {
		if !seen[name] {
			return fmt.Errorf("native archive is missing %s", name)
		}
	}
	return nil
}

func verifyInstallArchive(digest, file string) error {
	if err := verifyArchiveDigest(digest, file); err != nil {
		return err
	}
	reader, err := os.Open(file)
	if err != nil {
		return err
	}
	defer reader.Close()
	buffered := bufio.NewReader(reader)
	compressed, err := gzip.NewReader(buffered)
	if err != nil {
		return err
	}
	defer compressed.Close()
	compressed.Multistream(false)
	archive := tar.NewReader(compressed)
	seen, symlinks, regular := map[string]bool{}, map[string]bool{}, map[string]bool{}
	var expanded int64
	for {
		entry, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(entry.Name, "/")
		if name == "" || path.IsAbs(name) || path.Clean(name) != name || strings.Contains(name, `\`) || seen[name] || (name != "cq" && name != "CQNetwork.app" && !strings.HasPrefix(name, "CQNetwork.app/")) {
			return errors.New("install archive has an unsafe or duplicate entry")
		}
		seen[name] = true
		expanded += entry.Size
		if len(seen) > 10000 || entry.Size < 0 || entry.Size > 256<<20 || expanded > 512<<20 {
			return errors.New("install archive expansion exceeds size limit")
		}
		switch entry.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
			regular[name] = true
		case tar.TypeDir:
			if name == "cq" {
				return errors.New("install executable is not a regular file")
			}
		case tar.TypeSymlink:
			resolved := path.Clean(path.Join(path.Dir(name), entry.Linkname))
			if name == "cq" || entry.Linkname == "" || len(entry.Linkname) > 1024 || path.IsAbs(entry.Linkname) || strings.Contains(entry.Linkname, `\`) || !strings.HasPrefix(resolved, "CQNetwork.app/") {
				return errors.New("install archive symlink escapes the application")
			}
			symlinks[name] = true
		default:
			return errors.New("install archive has unsupported file type")
		}
	}
	for name := range seen {
		for ancestor := path.Dir(name); ancestor != "."; ancestor = path.Dir(ancestor) {
			if symlinks[ancestor] {
				return errors.New("install archive entry has a symlink ancestor")
			}
		}
	}
	for _, name := range []string{"cq", "CQNetwork.app/Contents/Info.plist", "CQNetwork.app/Contents/MacOS/CQNetwork", "CQNetwork.app/Contents/Library/SystemExtensions/dev.jacobcx.cq.network.proxy.systemextension/Contents/Info.plist", "CQNetwork.app/Contents/Library/SystemExtensions/dev.jacobcx.cq.network.proxy.systemextension/Contents/MacOS/CQNetworkProxy"} {
		if !regular[name] {
			return fmt.Errorf("install archive is missing regular file %s", name)
		}
	}
	// Verify gzip's checksum while allowing only bounded zero TAR padding.
	var padding [8192]byte
	for total := 0; ; {
		n, err := compressed.Read(padding[:])
		total += n
		if total > 1<<20 {
			return errors.New("install archive has excessive TAR padding")
		}
		for _, value := range padding[:n] {
			if value != 0 {
				return errors.New("install archive has trailing TAR content")
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if _, err := buffered.Peek(1); err != io.EOF {
		return errors.New("install archive has trailing content")
	}
	return nil
}

func renderCask(m manifest) string {
	download := fmt.Sprintf("  version %q\n  sha256 %q\n\n  url %q", m.Version, m.SHA256, strings.ReplaceAll(m.url(), m.Version, "#{version}"))
	if m.Bundles != nil {
		download = fmt.Sprintf("  arch arm: \"arm64\", intel: \"amd64\"\n\n  version %q\n  sha256 arm: %q,\n         intel: %q\n\n  url \"https://github.com/%s/releases/download/v#{version}/cq_#{version}_darwin_#{arch}_bundle.tar.gz\"", m.Version, m.Bundles.ARM64.SHA256, m.Bundles.AMD64.SHA256, repository)
	}
	return fmt.Sprintf(`cask "cq-network" do
%s
  name "CQ Network"
  desc "Native ChatGPT routing through CQ's configured WireGuard connection"
  homepage "https://github.com/jacobcxdev/cq"

  livecheck do
    skip "Native releases require reviewed signatures and notarisation"
  end

  depends_on macos: :ventura

  app "CQNetwork.app"

  uninstall launchctl: "dev.jacobcx.cq.network",
            script:    {
              executable: "#{appdir}/CQNetwork.app/Contents/MacOS/CQNetwork",
              args:       ["disable"],
            },
            delete:    "~/Library/LaunchAgents/dev.jacobcx.cq.network.plist"

  caveats <<~EOS
    To enable native ChatGPT routing, configure CQ's WireGuard profile and run:
      cq codex network enable
    macOS will ask you to approve CQ Network. ChatGPT is not restarted.
  EOS
end
`, download)
}
