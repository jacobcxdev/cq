package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/nativeusage"
)

func nativeUsageTestRunner() (nativeUsageRunner, *fsutil.MemFS) {
	fsys := fsutil.NewMemFS()
	return nativeUsageRunner{
		directory: func() (string, error) { return "/usage", nil },
		fs:        fsys, platform: "darwin",
		verifyTrust: func(context.Context, nativeusage.CertificateInfo) error { return nil },
	}, fsys
}

func TestNativeUsageCommandParsingAndHelp(t *testing.T) {
	for _, operation := range []string{"prepare", "enable", "status", "disable"} {
		t.Run(operation, func(t *testing.T) {
			var cli CLI
			parser, err := kong.New(&cli, kong.Writers(io.Discard, io.Discard))
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := parser.Parse([]string{"--json", "codex", "network", "usage", operation})
			if err != nil {
				t.Fatal(err)
			}
			if parsed.Command() != "codex network usage "+operation || !cli.JSON {
				t.Fatalf("command = %q", parsed.Command())
			}
			out := &bytes.Buffer{}
			handled, code, err := runPureGlobalInspection([]string{"codex", "network", "usage", operation, "--help"}, out, io.Discard)
			if !handled || code != 0 || err != nil || !strings.Contains(out.String(), "cq codex network usage "+operation) {
				t.Fatalf("help = %t,%d,%v: %s", handled, code, err, out)
			}
		})
	}
}

func TestNativeUsagePrepareDoesNotTrustOrEnable(t *testing.T) {
	runner, fsys := nativeUsageTestRunner()
	runner.verifyTrust = func(context.Context, nativeusage.CertificateInfo) error {
		t.Fatal("preparation checked operating-system trust")
		return nil
	}
	var output bytes.Buffer
	if err := runner.execute(context.Background(), "prepare", true, &output); err != nil {
		t.Fatal(err)
	}
	var status nativeUsageStatus
	if err := json.Unmarshal(output.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.Prepared || status.Enabled || status.Certificate == nil || status.Certificate.FingerprintSHA256 == "" {
		t.Fatalf("preparation status = %#v", status)
	}
	if config, err := nativeusage.LoadTLS("/usage", fsys); err != nil || config != nil {
		t.Fatalf("preparation enabled response handling: %v, %v", config, err)
	}
}

func TestNativeUsageEnableRequiresPreparation(t *testing.T) {
	runner, fsys := nativeUsageTestRunner()
	runner.verifyTrust = func(context.Context, nativeusage.CertificateInfo) error {
		t.Fatal("unprepared certificate reached trust check")
		return nil
	}
	if err := runner.execute(context.Background(), "enable", false, io.Discard); err == nil || !strings.Contains(err.Error(), "prepare") {
		t.Fatalf("unprepared enable = %v", err)
	}
	if _, err := fsys.Stat("/usage"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("enable created files: %v", err)
	}
}

func TestNativeUsageEnableChecksBothHostsBeforeMutation(t *testing.T) {
	for _, test := range []struct {
		name   string
		failAt int
	}{{"trusted", 0}, {"first host rejected", 1}, {"second host rejected", 2}} {
		t.Run(test.name, func(t *testing.T) {
			failAt := test.failAt
			runner, fsys := nativeUsageTestRunner()
			info, err := nativeusage.Prepare("/usage", fsys)
			if err != nil {
				t.Fatal(err)
			}
			var commands [][]string
			failure := errors.New("certificate trust rejected")
			runner.verifyTrust = func(ctx context.Context, info nativeusage.CertificateInfo) error {
				return verifyNativeUsageTrustWithCommand(ctx, info, func(_ context.Context, executable string, args ...string) error {
					commands = append(commands, append([]string{executable}, args...))
					_, enabled, err := nativeusage.Inspect("/usage", fsys)
					if err != nil || enabled {
						t.Fatalf("activation changed before trust checks finished: %t, %v", enabled, err)
					}
					if len(commands) == failAt {
						return failure
					}
					return nil
				})
			}
			var output bytes.Buffer
			err = runner.execute(context.Background(), "enable", true, &output)
			if failAt > 0 && !errors.Is(err, failure) || failAt == 0 && err != nil {
				t.Fatalf("enable error = %v", err)
			}
			want := [][]string{
				{"/usr/bin/security", "verify-cert", "-c", info.CertificatePath, "-p", "ssl", "-s", "chatgpt.com"},
				{"/usr/bin/security", "verify-cert", "-c", info.CertificatePath, "-p", "ssl", "-s", "ws.chatgpt.com"},
			}
			if failAt > 0 {
				want = want[:failAt]
			}
			if !reflect.DeepEqual(commands, want) {
				t.Fatalf("trust commands = %v, want %v", commands, want)
			}
			_, enabled, inspectErr := nativeusage.Inspect("/usage", fsys)
			if inspectErr != nil || enabled != (failAt == 0) {
				t.Fatalf("enabled = %t, %v", enabled, inspectErr)
			}
		})
	}
}

func TestNativeUsageStatusDoesNotCreateFiles(t *testing.T) {
	runner, fsys := nativeUsageTestRunner()
	runner.verifyTrust = func(context.Context, nativeusage.CertificateInfo) error {
		t.Fatal("status checked operating-system trust")
		return nil
	}
	var output bytes.Buffer
	if err := runner.execute(context.Background(), "status", true, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "{\"prepared\":false,\"enabled\":false}\n" {
		t.Fatalf("status = %s", output.String())
	}
	if _, err := fsys.Stat("/usage"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("status created files: %v", err)
	}
}

func TestNativeUsageDisablePreservesCertificateTrust(t *testing.T) {
	runner, fsys := nativeUsageTestRunner()
	info, err := nativeusage.Prepare("/usage", fsys)
	if err != nil {
		t.Fatal(err)
	}
	if err := nativeusage.SetEnabled("/usage", fsys, true); err != nil {
		t.Fatal(err)
	}
	runner.verifyTrust = func(context.Context, nativeusage.CertificateInfo) error {
		t.Fatal("disable checked or changed operating-system trust")
		return nil
	}
	var output bytes.Buffer
	if err := runner.execute(context.Background(), "disable", true, &output); err != nil {
		t.Fatal(err)
	}
	var status nativeUsageStatus
	if err := json.Unmarshal(output.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.Prepared || status.Enabled || !status.CertificateTrustUnchanged || status.Certificate.FingerprintSHA256 != info.FingerprintSHA256 {
		t.Fatalf("disable status = %#v", status)
	}
	output.Reset()
	if err := runner.execute(context.Background(), "disable", false, &output); err != nil || !strings.Contains(output.String(), "remove it separately") {
		t.Fatalf("disable message = %q, %v", output.String(), err)
	}
}

func TestNativeUsageEnableIsMacOSOnly(t *testing.T) {
	runner, fsys := nativeUsageTestRunner()
	runner.platform = "linux"
	if err := runner.execute(context.Background(), "enable", false, io.Discard); err == nil || !strings.Contains(err.Error(), "macOS") {
		t.Fatalf("unsupported enable = %v", err)
	}
	if _, err := fsys.Stat("/usage"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsupported enable created files: %v", err)
	}
}

func TestNativeUsageRejectsUnexpectedCertificateScope(t *testing.T) {
	for _, hosts := range [][]string{nil, {"chatgpt.com"}, {"chatgpt.com", "chatgpt.com"}, {"chatgpt.com", "example.com"}} {
		info := nativeusage.CertificateInfo{CertificatePath: "/usage/usage-cert.pem", Hosts: hosts}
		err := verifyNativeUsageTrustWithCommand(context.Background(), info, func(context.Context, string, ...string) error {
			t.Fatal("unexpected hosts reached operating-system trust check")
			return nil
		})
		if err == nil {
			t.Fatalf("invalid scope accepted: %v", hosts)
		}
	}
}

func TestNativeUsageUnknownOperationDoesNotResolveDirectory(t *testing.T) {
	runner, _ := nativeUsageTestRunner()
	runner.directory = func() (string, error) {
		t.Fatal("unknown operation resolved the certificate directory")
		return "", nil
	}
	if err := runner.execute(context.Background(), "trust-everything", false, io.Discard); err == nil {
		t.Fatal("unknown operation accepted")
	}
}
