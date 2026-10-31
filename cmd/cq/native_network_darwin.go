//go:build darwin

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
)

// This requirement accepts authorised development and distribution builds from
// the company team, but never an ad-hoc binary or a similarly named application.
const nativeNetworkSigningRequirement = `anchor apple generic and identifier "` + nativeNetworkIdentifier + `" and certificate leaf[subject.OU] = "` + nativeNetworkTeam + `"`

type nativeNetworkAgent struct {
	home       string
	uid        int
	executable string
	run        func(context.Context, string, ...string) ([]byte, error)
}

func defaultNativeNetworkRunner() nativeNetworkRunner {
	command := func(ctx context.Context, executable string, arguments ...string) ([]byte, error) {
		timeout := 12 * time.Second
		if len(arguments) > 0 && (arguments[0] == "activate" || arguments[0] == "disable") {
			timeout = 30 * time.Second
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, executable, arguments...)
		output := nativeNetworkOutput{}
		cmd.Stdout = &output
		// Keep stderr separate: status stdout is an authenticated JSON contract,
		// and diagnostics from an external process may contain private paths.
		cmd.Stderr = io.Discard
		err := cmd.Run()
		if output.truncated {
			return nil, errors.New("native network command output exceeded size limit")
		}
		if ctx.Err() != nil {
			return output.Bytes(), fmt.Errorf("%s timed out: %w", filepath.Base(executable), ctx.Err())
		}
		return output.Bytes(), err
	}
	agent := func() (nativeNetworkAgent, error) {
		home, err := os.UserHomeDir()
		if err != nil {
			return nativeNetworkAgent{}, fmt.Errorf("resolve user home: %w", err)
		}
		executable, err := dotsExecutable(os.Args[0])
		if err != nil {
			return nativeNetworkAgent{}, fmt.Errorf("resolve CQ executable: %w", err)
		}
		return nativeNetworkAgent{home: home, uid: os.Getuid(), executable: executable, run: command}, nil
	}
	return nativeNetworkRunner{
		validateBroker: func(context.Context) error {
			if err := nativeNetworkPlatformSupported(); err != nil {
				return err
			}
			_, err := proxy.NativeEgressDescriptorPath(fsutil.OSFileSystem{})
			return err
		},
		verifyHost: func(ctx context.Context) error {
			if err := nativeNetworkPlatformSupported(); err != nil {
				return err
			}
			if err := validateDarwinServiceExecutable(nativeNetworkExecutable); err != nil {
				return fmt.Errorf("CQ Network companion unavailable; install the signed macOS companion: %w", err)
			}
			if _, err := command(ctx, "/usr/bin/codesign", "--verify", "--strict", "--all-architectures", "-R="+nativeNetworkSigningRequirement, nativeNetworkHostBundle); err != nil {
				return fmt.Errorf("CQ Network signature does not match the trusted company application: %w", err)
			}
			return nil
		},
		installAgent: func(ctx context.Context) error {
			platform, err := agent()
			if err != nil {
				return err
			}
			return platform.install(ctx)
		},
		removeAgent: func(ctx context.Context) error {
			platform, err := agent()
			if err != nil {
				return err
			}
			return platform.remove(ctx)
		},
		host: func(ctx context.Context, arguments ...string) ([]byte, error) {
			return command(ctx, nativeNetworkExecutable, arguments...)
		},
		wait: waitNativeNetwork,
	}
}

func nativeNetworkPlatformSupported() error {
	release, err := syscall.Sysctl("kern.osrelease")
	if err != nil {
		return errors.New("native ChatGPT network routing could not determine macOS version")
	}
	return validateNativeNetworkKernel(release)
}

func validateNativeNetworkKernel(release string) error {
	major, err := strconv.Atoi(strings.SplitN(release, ".", 2)[0])
	if err != nil || major < 22 {
		return errors.New("native ChatGPT network routing requires macOS 13 Ventura or later; core CQ remains available")
	}
	return nil
}

type nativeNetworkOutput struct {
	buffer    bytes.Buffer
	truncated bool
}

func (output *nativeNetworkOutput) Write(data []byte) (int, error) {
	count := min(len(data), (16<<10)-output.Len())
	_, _ = output.buffer.Write(data[:count])
	output.truncated = output.truncated || count != len(data)
	return len(data), nil
}

func (output *nativeNetworkOutput) Len() int      { return output.buffer.Len() }
func (output *nativeNetworkOutput) Bytes() []byte { return output.buffer.Bytes() }

func (agent nativeNetworkAgent) paths() (plist, log, domain, target string) {
	plist = filepath.Join(agent.home, "Library", "LaunchAgents", nativeNetworkIdentifier+".plist")
	log = filepath.Join(agent.home, "Library", "Logs", "cq", "native-network.log")
	domain = "gui/" + strconv.Itoa(agent.uid)
	target = domain + "/" + nativeNetworkIdentifier
	return
}

func (agent nativeNetworkAgent) definition() ([]byte, error) {
	if err := agent.validateHome(); err != nil {
		return nil, err
	}
	if err := validateDarwinServiceExecutable(agent.executable); err != nil {
		return nil, err
	}
	_, log, _, _ := agent.paths()
	return renderDarwinLaunchAgent(darwinLaunchAgentDefinition{
		Label:            nativeNetworkIdentifier,
		ProgramArguments: []string{nativeNetworkExecutable, "run", "--cq", agent.executable},
		RunAtLoad:        true, KeepAlive: true, ExitTimeOut: 10,
		StandardErrorPath: log,
	})
}

func (agent nativeNetworkAgent) install(ctx context.Context) error {
	definition, err := agent.definition()
	if err != nil {
		return err
	}
	plist, log, domain, target := agent.paths()
	existing, err := readNativeNetworkAgent(plist)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, printErr := agent.run(ctx, "/bin/launchctl", "print", target)
	migrate := false
	if printErr == nil {
		if bytes.Equal(existing, definition) {
			return nil
		}
		legacy := bytes.Replace(definition, []byte("<key>ProcessType</key>\n\t<string>Interactive</string>"), []byte("<key>ProcessType</key>\n\t<string>Background</string>"), 1)
		if !bytes.Equal(existing, legacy) {
			return errors.New("CQ Network LaunchAgent differs from the installed definition; disable it before enabling again")
		}
		migrate = true
	} else if !isDarwinLaunchctlNotLoaded(printErr) {
		return fmt.Errorf("inspect CQ Network LaunchAgent: %w", printErr)
	}
	if err := prepareNativeNetworkLogDirectory(filepath.Dir(log)); err != nil {
		return err
	}
	if migrate {
		if _, err := agent.run(ctx, "/bin/launchctl", "bootout", target); err != nil && !isDarwinLaunchctlNotLoaded(err) {
			return fmt.Errorf("stop legacy CQ Network user broker: %w", err)
		}
	}
	restore := func(failure error) error {
		if !migrate {
			return failure
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 12*time.Second)
		defer cancel()
		var restoreErr error
		if _, err := agent.run(cleanupCtx, "/bin/launchctl", "bootout", target); err != nil && !isDarwinLaunchctlNotLoaded(err) {
			restoreErr = fmt.Errorf("stop failed CQ Network user broker: %w", err)
		}
		current, readErr := readNativeNetworkAgent(plist)
		if readErr != nil || !bytes.Equal(current, existing) {
			if err := atomicWriteDarwinLaunchAgent(plist, existing); err != nil {
				return errors.Join(failure, restoreErr, fmt.Errorf("restore previous CQ Network definition: %w", err))
			}
		}
		if _, err := agent.run(cleanupCtx, "/bin/launchctl", "bootstrap", domain, plist); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restore previous CQ Network user broker: %w", err))
		}
		return errors.Join(failure, restoreErr)
	}
	if err := atomicWriteDarwinLaunchAgent(plist, definition); err != nil {
		return restore(err)
	}
	if _, err := agent.run(ctx, "/bin/launchctl", "bootstrap", domain, plist); err != nil {
		return restore(fmt.Errorf("start CQ Network user broker: %w", err))
	}
	return nil
}

func (agent nativeNetworkAgent) remove(ctx context.Context) error {
	if err := agent.validateHome(); err != nil {
		return err
	}
	plist, _, _, target := agent.paths()
	if _, err := readNativeNetworkAgent(plist); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := agent.run(ctx, "/bin/launchctl", "bootout", target); err != nil && !isDarwinLaunchctlNotLoaded(err) {
		return fmt.Errorf("stop CQ Network user broker: %w", err)
	}
	if err := os.Remove(plist); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove CQ Network LaunchAgent: %w", err)
	}
	return nil
}

func (agent nativeNetworkAgent) validateHome() error {
	if !filepath.IsAbs(agent.home) || filepath.Clean(agent.home) != agent.home {
		return errors.New("native network user home must be a clean absolute path")
	}
	return nil
}

func readNativeNetworkAgent(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	inspector := fsutil.OSFileSystem{}
	owner, owned := inspector.FileOwnerUID(info)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !owned || owner != inspector.EffectiveUID() || info.Size() > maxDarwinPlistBytes {
		return nil, errors.New("CQ Network LaunchAgent must be a private user-owned regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	definition, err := parseDarwinLaunchAgent(data)
	if err != nil || definition.Label != nativeNetworkIdentifier {
		return nil, errors.New("CQ Network LaunchAgent has an unexpected definition")
	}
	return data, nil
}

func prepareNativeNetworkLogDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create CQ Network log directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	inspector := fsutil.OSFileSystem{}
	owner, owned := inspector.FileOwnerUID(info)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 || !owned || owner != inspector.EffectiveUID() {
		return errors.New("CQ Network log directory must be a user-owned directory without group or other write access")
	}
	return nil
}
