package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"
)

const (
	packageIdentifier = "jacobcxdev.cq"
	repositoryModule  = "module github.com/jacobcxdev/cq"
)

var (
	versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	digestPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	templateNames  = []string{
		"jacobcxdev.cq.yaml.tmpl",
		"jacobcxdev.cq.installer.yaml.tmpl",
		"jacobcxdev.cq.locale.en-US.yaml.tmpl",
	}
)

type manifestConfig struct {
	Version     string
	X64URL      string
	X64SHA256   string
	ARM64URL    string
	ARM64SHA256 string
}

func (config manifestConfig) Validate() error {
	if !versionPattern.MatchString(config.Version) {
		return fmt.Errorf("version must be stable semantic version")
	}
	if !digestPattern.MatchString(config.X64SHA256) || !digestPattern.MatchString(config.ARM64SHA256) {
		return fmt.Errorf("installer digests must be lowercase SHA-256")
	}
	if err := validateInstallerURL(config.X64URL, config.Version, "amd64"); err != nil {
		return fmt.Errorf("x64 installer URL: %w", err)
	}
	if err := validateInstallerURL(config.ARM64URL, config.Version, "arm64"); err != nil {
		return fmt.Errorf("arm64 installer URL: %w", err)
	}
	return nil
}

func validateInstallerURL(rawURL, version, architecture string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse URL: %w", err)
	}
	wantPath := fmt.Sprintf(
		"/jacobcxdev/cq/releases/download/v%s/cq_%s_windows_%s.msi",
		version,
		version,
		architecture,
	)
	if parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "github.com") || parsed.Port() != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.EscapedPath() != wantPath {
		return fmt.Errorf("URL must be pinned to the expected GitHub release asset")
	}
	return nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "wingetmanifest: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("wingetmanifest", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var config manifestConfig
	var output string
	flags.StringVar(&config.Version, "version", "", "release version")
	flags.StringVar(&config.X64URL, "x64-url", "", "x64 installer URL")
	flags.StringVar(&config.X64SHA256, "x64-sha256", "", "x64 installer SHA-256")
	flags.StringVar(&config.ARM64URL, "arm64-url", "", "arm64 installer URL")
	flags.StringVar(&config.ARM64SHA256, "arm64-sha256", "", "arm64 installer SHA-256")
	flags.StringVar(&output, "output", "", "empty output directory")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return fmt.Errorf("invalid manifest generator arguments")
	}
	return generate(config, output)
}

func generate(config manifestConfig, output string) (resultErr error) {
	if err := config.Validate(); err != nil {
		return err
	}
	if output == "" {
		return fmt.Errorf("output directory is required")
	}
	output, err := filepath.Abs(output)
	if err != nil {
		return fmt.Errorf("resolve output directory: %w", err)
	}
	output = filepath.Clean(output)
	if err := ensureEmptyOutput(output); err != nil {
		return err
	}
	manifests, err := renderManifests(config)
	if err != nil {
		return err
	}
	destination := filepath.Join(output, "manifests", "j", "jacobcxdev", "cq", config.Version)
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return fmt.Errorf("create manifest destination: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			resultErr = errors.Join(resultErr, os.RemoveAll(filepath.Join(output, "manifests")))
		}
	}()
	names := make([]string, 0, len(manifests))
	for name := range manifests {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(destination, name)
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return fmt.Errorf("create %s: %w", name, err)
		}
		body := manifests[name]
		if _, err := file.Write(body); err != nil {
			_ = file.Close()
			return fmt.Errorf("write %s: %w", name, err)
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return fmt.Errorf("sync %s: %w", name, err)
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("close %s: %w", name, err)
		}
	}
	committed = true
	return nil
}

func ensureEmptyOutput(output string) error {
	info, err := os.Lstat(output)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(output, 0o755); err != nil {
			return fmt.Errorf("create output directory: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect output directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("output must be a real directory")
	}
	entries, err := os.ReadDir(output)
	if err != nil {
		return fmt.Errorf("read output directory: %w", err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("output directory must be empty")
	}
	return nil
}

func renderManifests(config manifestConfig) (map[string][]byte, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	repositoryRoot, err := findRepositoryRoot()
	if err != nil {
		return nil, err
	}
	templateRoot := filepath.Join(repositoryRoot, "packaging", "winget")
	manifests := make(map[string][]byte, len(templateNames))
	for _, name := range templateNames {
		templatePath := filepath.Join(templateRoot, name)
		parsed, err := template.New(name).Option("missingkey=error").ParseFiles(templatePath)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		var output bytes.Buffer
		if err := parsed.ExecuteTemplate(&output, name, config); err != nil {
			return nil, fmt.Errorf("render %s: %w", name, err)
		}
		manifests[strings.TrimSuffix(name, ".tmpl")] = output.Bytes()
	}
	return manifests, nil
}

func findRepositoryRoot() (string, error) {
	directory, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		module, readErr := os.ReadFile(filepath.Join(directory, "go.mod"))
		if readErr == nil && strings.HasPrefix(string(module), repositoryModule+"\n") {
			return directory, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", fmt.Errorf("CQ repository root was not found")
		}
		directory = parent
	}
}
