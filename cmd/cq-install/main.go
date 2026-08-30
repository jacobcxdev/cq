package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/installer"
	"github.com/jacobcxdev/cq/internal/installstate"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

const (
	cqModulePath          = "github.com/jacobcxdev/cq"
	maxVersionOutputBytes = 4 << 10
)

var (
	version               = ""
	installerVersionRegex = regexp.MustCompile(`^v?([0-9]+\.[0-9]+\.[0-9]+)$`)
)

type installerAction interface {
	Install(context.Context) error
	Uninstall(context.Context) error
}

type commandDependencies struct {
	ResolveVersion func() (string, error)
	Build          func(context.Context, installstate.Owner, string) (installerAction, error)
	Output         io.Writer
	AllowWinGet    bool
}

type installerCommand struct {
	Action string
	Owner  installstate.Owner
	Silent bool
	Help   bool
}

func main() {
	if err := runInstaller(context.Background(), os.Args[1:], defaultCommandDependencies()); err != nil {
		fmt.Fprintf(os.Stderr, "cq-install: %v\n", err)
		os.Exit(1)
	}
}

func defaultCommandDependencies() commandDependencies {
	return commandDependencies{
		ResolveVersion: effectiveVersion,
		Build:          buildInstaller,
		Output:         os.Stdout,
		AllowWinGet:    runtime.GOOS == "windows" && strings.TrimSpace(version) != "",
	}
}

func runInstaller(ctx context.Context, args []string, deps commandDependencies) error {
	if deps.ResolveVersion == nil || deps.Build == nil || deps.Output == nil {
		return fmt.Errorf("installer dependencies are unavailable")
	}
	command, err := parseInstallerCommand(args, deps.AllowWinGet)
	if err != nil {
		return err
	}
	if command.Help {
		_, err := io.WriteString(deps.Output, installerHelp)
		return err
	}
	releaseVersion, err := deps.ResolveVersion()
	if err != nil {
		return err
	}
	action, err := deps.Build(ctx, command.Owner, releaseVersion)
	if err != nil {
		return err
	}
	if action == nil {
		return fmt.Errorf("installer action is unavailable")
	}
	switch command.Action {
	case "install":
		err = action.Install(ctx)
	case "uninstall":
		err = action.Uninstall(ctx)
	default:
		return fmt.Errorf("unsupported installer action")
	}
	if err != nil {
		return err
	}
	if command.Silent {
		return nil
	}
	if command.Action == "install" {
		_, err = fmt.Fprintf(deps.Output, "CQ %s installed with proxy and refresh services.\n", releaseVersion)
	} else {
		_, err = io.WriteString(deps.Output, "CQ uninstalled; user data preserved.\n")
	}
	return err
}

func parseInstallerCommand(args []string, allowWinGet bool) (installerCommand, error) {
	command := installerCommand{Action: "install", Owner: installstate.OwnerGo}
	actionSet := false
	ownerSet := false
	for _, argument := range args {
		switch {
		case argument == "--help" || argument == "-h":
			if len(args) != 1 {
				return installerCommand{}, fmt.Errorf("help does not accept other arguments")
			}
			command.Help = true
			return command, nil
		case argument == "install" || argument == "uninstall":
			if actionSet {
				return installerCommand{}, fmt.Errorf("multiple installer actions")
			}
			command.Action = argument
			actionSet = true
		case argument == "--silent":
			if command.Silent {
				return installerCommand{}, fmt.Errorf("duplicate silent option")
			}
			command.Silent = true
		case strings.HasPrefix(argument, "--owner="):
			if ownerSet {
				return installerCommand{}, fmt.Errorf("duplicate package owner")
			}
			owner := installstate.Owner(strings.TrimPrefix(argument, "--owner="))
			if owner == installstate.OwnerWinGet && allowWinGet {
				command.Owner = owner
			} else if owner != installstate.OwnerGo {
				return installerCommand{}, fmt.Errorf("unsupported package owner")
			}
			ownerSet = true
		default:
			return installerCommand{}, fmt.Errorf("unsupported installer argument")
		}
	}
	return command, nil
}

const installerHelp = `Usage:
  cq-install [install|uninstall] [--silent]

Downloads and verifies tagged CQ release binaries, then manages proxy and
refresh services as one complete per-user installation. User data is preserved
on uninstall.
`

func effectiveVersion() (string, error) {
	info, ok := debug.ReadBuildInfo()
	return effectiveVersionFrom(version, info, ok)
}

func effectiveVersionFrom(linked string, info *debug.BuildInfo, ok bool) (string, error) {
	if strings.TrimSpace(linked) != "" {
		return normaliseVersion(linked)
	}
	if !ok || info == nil || info.Main.Path != cqModulePath || info.Main.Replace != nil || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return "", fmt.Errorf("cq-install requires a tagged, unreplaced module version")
	}
	return normaliseVersion(info.Main.Version)
}

func normaliseVersion(value string) (string, error) {
	matches := installerVersionRegex.FindStringSubmatch(value)
	if matches == nil {
		return "", fmt.Errorf("cq-install requires a stable semantic version")
	}
	return matches[1], nil
}

func buildInstaller(ctx context.Context, owner installstate.Owner, releaseVersion string) (installerAction, error) {
	roots, err := userdirs.Default()
	if err != nil {
		return nil, err
	}
	sourceInstaller, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve installer executable: %w", err)
	}
	sourceInstaller, err = filepath.Abs(sourceInstaller)
	if err != nil {
		return nil, fmt.Errorf("resolve absolute installer executable: %w", err)
	}
	sourceInstaller = filepath.Clean(sourceInstaller)
	platform, err := resolvePlatformInstallation(ctx, owner, roots, sourceInstaller)
	if err != nil {
		return nil, err
	}
	release, err := installer.NewRelease(releaseVersion, runtime.GOOS, runtime.GOARCH, nil)
	if err != nil {
		return nil, err
	}
	fsys := fsutil.OSFileSystem{}
	store := &installstate.Store{FS: fsys, Roots: roots}
	return &installer.Installer{
		FS:         fsys,
		Downloader: release,
		Runner: commandVersionRunner{
			Run: runCommandVersion,
		},
		Classifier: installer.BinaryClassifier{FS: fsys, State: store},
		Lifecycle: commandLifecycle{
			Executable: platform.Executable,
			Owner:      owner,
			Run:        runCQService,
		},
		Metadata: platform.Metadata,
		State:    store,
		Locker: installer.FileInstallLocker{
			FS:        fsys,
			StateRoot: roots.State,
		},
		Temporary: installer.OSTemporaryDirectories{Root: filepath.Join(roots.Cache, "installer")},
		Installation: installer.Installation{
			Owner:      owner,
			Version:    releaseVersion,
			Executable: platform.Executable,
			Services:   platform.Services,
		},
	}, nil
}

type platformInstallation struct {
	Executable string
	Services   []string
	Metadata   installer.PlatformMetadata
}

type noPlatformMetadata struct{}

func (noPlatformMetadata) Install(context.Context, installer.Installation) error { return nil }
func (noPlatformMetadata) Remove(context.Context, installer.Installation) error  { return nil }
func (noPlatformMetadata) Inspect(context.Context, installer.Installation) error { return nil }

type commandLifecycle struct {
	Executable string
	Owner      installstate.Owner
	Run        func(context.Context, string, ...string) error
}

func (lifecycle commandLifecycle) Stop(ctx context.Context) error {
	return lifecycle.run(ctx, "service", "uninstall", "--owner="+string(lifecycle.Owner))
}

func (lifecycle commandLifecycle) Install(ctx context.Context, owner installstate.Owner) error {
	if owner != lifecycle.Owner {
		return fmt.Errorf("service owner changed during installation")
	}
	return lifecycle.run(ctx, "service", "install", "--owner="+string(owner))
}

func (lifecycle commandLifecycle) Status(ctx context.Context) error {
	return lifecycle.run(ctx, "service", "status", "--json")
}

func (lifecycle commandLifecycle) Uninstall(ctx context.Context, owner installstate.Owner) error {
	if owner != lifecycle.Owner {
		return fmt.Errorf("service owner changed during installation")
	}
	return lifecycle.run(ctx, "service", "uninstall", "--owner="+string(owner))
}

func (lifecycle commandLifecycle) run(ctx context.Context, args ...string) error {
	if lifecycle.Executable == "" || lifecycle.Run == nil {
		return fmt.Errorf("service command is unavailable")
	}
	return lifecycle.Run(ctx, lifecycle.Executable, args...)
}

func runCQService(ctx context.Context, executable string, args ...string) error {
	command := exec.CommandContext(ctx, executable, args...)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return fmt.Errorf("CQ service command failed: %w", err)
	}
	return nil
}

type commandVersionRunner struct {
	Run func(context.Context, string) (string, error)
}

func (runner commandVersionRunner) Version(ctx context.Context, executable string) (string, error) {
	if runner.Run == nil {
		return "", fmt.Errorf("version command is unavailable")
	}
	value, err := runner.Run(ctx, executable)
	if err != nil {
		return "", err
	}
	return normaliseVersion(strings.TrimSpace(value))
}

func runCommandVersion(ctx context.Context, executable string) (string, error) {
	var output boundedBuffer
	output.Limit = maxVersionOutputBytes
	command := exec.CommandContext(ctx, executable, "--version")
	command.Stdout = &output
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("CQ version command failed: %w", err)
	}
	if output.Exceeded {
		return "", fmt.Errorf("CQ version output exceeds size limit")
	}
	return output.Buffer.String(), nil
}

type boundedBuffer struct {
	Buffer   bytes.Buffer
	Limit    int
	Exceeded bool
}

func (buffer *boundedBuffer) Write(data []byte) (int, error) {
	original := len(data)
	remaining := buffer.Limit - buffer.Buffer.Len()
	if remaining <= 0 {
		buffer.Exceeded = true
		return original, nil
	}
	if len(data) > remaining {
		data = data[:remaining]
		buffer.Exceeded = true
	}
	_, err := buffer.Buffer.Write(data)
	return original, err
}

var _ installer.Lifecycle = commandLifecycle{}
var _ installer.VersionRunner = commandVersionRunner{}
var _ installer.PlatformMetadata = noPlatformMetadata{}
