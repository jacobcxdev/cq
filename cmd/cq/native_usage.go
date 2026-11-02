package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/nativeusage"
	"github.com/jacobcxdev/cq/internal/proxy"
)

// CodexNetworkUsageCmd manages normal-launch usage response handling.
type CodexNetworkUsageCmd struct {
	Prepare struct{} `cmd:"" help:"Prepare a private usage certificate without enabling it"`
	Enable  struct{} `cmd:"" help:"Enable usage response handling after certificate trust is approved"`
	Status  struct{} `cmd:"" help:"Show usage certificate and response-handling settings"`
	Disable struct{} `cmd:"" help:"Disable usage response handling without removing certificate trust"`
}

type nativeUsageStatus struct {
	Prepared                  bool                         `json:"prepared"`
	Enabled                   bool                         `json:"enabled"`
	Certificate               *nativeusage.CertificateInfo `json:"certificate,omitempty"`
	CertificateTrustUnchanged bool                         `json:"certificate_trust_unchanged,omitempty"`
}

type nativeUsageRunner struct {
	directory   func() (string, error)
	fs          fsutil.FileSystem
	platform    string
	verifyTrust func(context.Context, nativeusage.CertificateInfo) error
}

func defaultNativeUsageRunner() nativeUsageRunner {
	return nativeUsageRunner{
		directory: func() (string, error) {
			paths, err := proxy.ResolveDefaultPaths()
			if err != nil {
				return "", err
			}
			return filepath.Join(paths.RuntimeDir, "native-usage"), nil
		},
		fs: fsutil.OSFileSystem{}, platform: runtime.GOOS, verifyTrust: verifyNativeUsageTrust,
	}
}

func (runner nativeUsageRunner) execute(ctx context.Context, operation string, jsonOutput bool, output io.Writer) error {
	if operation != "prepare" && operation != "enable" && operation != "status" && operation != "disable" {
		return fmt.Errorf("unknown native usage operation: %s", operation)
	}
	if operation == "enable" && runner.platform != "darwin" {
		return errors.New("native ChatGPT usage response handling is only supported on macOS")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	root, err := runner.directory()
	if err != nil {
		return err
	}
	switch operation {
	case "prepare":
		if _, err = nativeusage.Prepare(root, runner.fs); err != nil {
			return err
		}
	case "enable":
		info, _, inspectErr := nativeusage.Inspect(root, runner.fs)
		if inspectErr != nil {
			return inspectErr
		}
		if info.CertificatePath == "" {
			return errors.New("usage certificate is not prepared; run cq codex network usage prepare")
		}
		if err = runner.verifyTrust(ctx, info); err != nil {
			return err
		}
		if err = nativeusage.SetEnabled(root, runner.fs, true); err != nil {
			return err
		}
	case "disable":
		if err = nativeusage.SetEnabled(root, runner.fs, false); err != nil {
			return err
		}
	}
	info, enabled, err := nativeusage.Inspect(root, runner.fs)
	if err != nil {
		return err
	}
	status := nativeUsageStatus{Prepared: info.CertificatePath != "", Enabled: enabled, CertificateTrustUnchanged: operation == "disable"}
	if status.Prepared {
		status.Certificate = &info
	}
	return writeNativeUsageStatus(output, operation, jsonOutput, status)
}

func writeNativeUsageStatus(output io.Writer, operation string, jsonOutput bool, status nativeUsageStatus) error {
	if jsonOutput {
		return json.NewEncoder(output).Encode(status)
	}
	var message string
	switch operation {
	case "prepare":
		message = "Native ChatGPT usage certificate prepared. Certificate trust is configured separately."
	case "enable":
		message = "Native ChatGPT usage response handling enabled for new connections."
	case "disable":
		message = "Native ChatGPT usage response handling disabled. Certificate trust is unchanged; remove it separately when no longer needed."
	default:
		switch {
		case !status.Prepared:
			message = "Native ChatGPT usage response handling is not prepared."
		case status.Enabled:
			message = "Native ChatGPT usage response handling is enabled."
		default:
			message = "Native ChatGPT usage response handling is disabled."
		}
	}
	if _, err := fmt.Fprintln(output, message); err != nil {
		return err
	}
	if status.Certificate != nil {
		info := status.Certificate
		_, err := fmt.Fprintf(output, "Certificate: %s\nSHA-256: %s\nHosts: %s\nExpires: %s\n", info.CertificatePath, info.FingerprintSHA256, strings.Join(info.Hosts, ", "), info.NotAfter.Format(time.RFC3339))
		return err
	}
	return nil
}

type nativeUsageTrustCommand func(context.Context, string, ...string) error

func verifyNativeUsageTrustWithCommand(ctx context.Context, info nativeusage.CertificateInfo, command nativeUsageTrustCommand) error {
	if info.CertificatePath == "" || len(info.Hosts) != 2 || info.Hosts[0] == info.Hosts[1] || !nativeusage.IsHost(info.Hosts[0]) || !nativeusage.IsHost(info.Hosts[1]) {
		return errors.New("usage certificate host scope is invalid")
	}
	for _, host := range info.Hosts {
		if err := command(ctx, "/usr/bin/security", "verify-cert", "-c", info.CertificatePath, "-p", "ssl", "-s", host); err != nil {
			return fmt.Errorf("usage certificate is not trusted for %s; approve its macOS SSL trust before enabling: %w", host, err)
		}
	}
	return nil
}
