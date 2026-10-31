// Command nativepackage validates the separately signed macOS companion release.
package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
)

const repository = "jacobcxdev/cq"

var (
	versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	commitPattern  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	digestPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	macOSPattern   = regexp.MustCompile(`^[1-9][0-9]*\.[0-9]+(?:\.[0-9]+)?$`)
)

type manifest struct {
	SchemaVersion int    `json:"schema_version"`
	Version       string `json:"version"`
	SourceCommit  string `json:"source_commit"`
	AssetID       int64  `json:"asset_id"`
	SHA256        string `json:"sha256"`
	MinimumMacOS  string `json:"minimum_macos"`
}

func (m manifest) tag() string   { return "cq-network-v" + m.Version }
func (m manifest) asset() string { return "CQNetwork_" + m.Version + "_universal.zip" }
func (m manifest) url() string {
	return "https://github.com/" + repository + "/releases/download/" + m.tag() + "/" + m.asset()
}
func (m manifest) validate() error {
	if m.SchemaVersion != 1 || !versionPattern.MatchString(m.Version) || !commitPattern.MatchString(m.SourceCommit) || m.AssetID <= 0 || !digestPattern.MatchString(m.SHA256) || !macOSPattern.MatchString(m.MinimumMacOS) {
		return errors.New("native release manifest has invalid or missing identity, source, checksum or minimum macOS")
	}
	return nil
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "nativepackage:", err)
		os.Exit(1)
	}
}

func run(args []string, output io.Writer) error {
	if len(args) == 6 && args[0] == "pin" {
		id, err := strconv.ParseInt(args[3], 10, 64)
		if err != nil {
			return errors.New("invalid native release asset ID")
		}
		m := manifest{1, args[1], args[2], id, args[4], args[5]}
		if err := m.validate(); err != nil {
			return err
		}
		encoder := json.NewEncoder(output)
		encoder.SetIndent("", "  ")
		return encoder.Encode(m)
	}
	if len(args) < 2 {
		return errors.New("usage: nativepackage info MANIFEST | verify MANIFEST ZIP RELEASE_JSON [--allow-draft] | published MANIFEST RELEASE_JSON | cask MANIFEST OUTPUT | pin VERSION SOURCE_COMMIT ASSET_ID SHA256 MINIMUM_MACOS")
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
		return json.NewEncoder(output).Encode(map[string]any{"tag": m.tag(), "asset": m.asset(), "asset_id": m.AssetID, "url": m.url(), "version": m.Version, "source_commit": m.SourceCommit, "minimum_macos": m.MinimumMacOS})
	case args[0] == "verify" && (len(args) == 4 || len(args) == 5 && args[4] == "--allow-draft"):
		if err := verifyRelease(m, args[3], len(args) == 5); err != nil {
			return err
		}
		return verifyArchive(m, args[2])
	case args[0] == "published" && len(args) == 3:
		return verifyRelease(m, args[2], false)
	case args[0] == "cask" && len(args) == 3:
		return os.WriteFile(args[2], []byte(renderCask(m)), 0o644)
	default:
		return errors.New("invalid nativepackage arguments")
	}
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
	if release.Tag != m.tag() || release.Target != m.SourceCommit || release.Draft && !allowDraft || !release.Prerelease {
		return errors.New("native release tag, source commit or publication state differs from reviewed manifest")
	}
	count := 0
	for _, asset := range release.Assets {
		if asset.Name != m.asset() {
			continue
		}
		count++
		if asset.ID != m.AssetID || asset.Digest != "sha256:"+m.SHA256 || asset.State != "uploaded" || asset.URL != m.url() {
			return errors.New("native release asset identity or digest differs from reviewed manifest")
		}
	}
	if count != 1 {
		return errors.New("native release asset is absent or ambiguous")
	}
	return nil
}

func verifyArchive(m manifest, file string) error {
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
	if hex.EncodeToString(hash.Sum(nil)) != m.SHA256 {
		return errors.New("native archive SHA-256 differs from reviewed manifest")
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
	for _, name := range []string{"CQNetwork.app/Contents/Info.plist", "CQNetwork.app/Contents/MacOS/CQNetwork", "CQNetwork.app/Contents/Library/SystemExtensions/CQNetworkProxy.systemextension/Contents/Info.plist", "CQNetwork.app/Contents/Library/SystemExtensions/CQNetworkProxy.systemextension/Contents/MacOS/CQNetworkProxy"} {
		if !seen[name] {
			return fmt.Errorf("native archive is missing %s", name)
		}
	}
	return nil
}

func renderCask(m manifest) string {
	return fmt.Sprintf(`cask "cq-network" do
  version "%s"
  sha256 "%s"

  url "%s"
  name "CQ Network"
  desc "Native ChatGPT routing through CQ's configured WireGuard connection"
  homepage "https://github.com/jacobcxdev/cq"

  livecheck do
    skip "Native releases require reviewed signatures and notarisation"
  end

  depends_on macos: ">= %s"

  app "CQNetwork.app"

  uninstall script: {
    executable: "#{appdir}/CQNetwork.app/Contents/MacOS/CQNetwork",
    args:       ["disable"],
  },
  launchctl: "dev.jacobcx.cq.network",
  delete:    "~/Library/LaunchAgents/dev.jacobcx.cq.network.plist"

  caveats <<~EOS
    To enable native ChatGPT routing, configure CQ's WireGuard profile and run:
      cq codex network enable
    macOS will ask you to approve CQ Network. ChatGPT is not restarted.
  EOS
end
`, m.Version, m.SHA256, m.url(), m.MinimumMacOS)
}
