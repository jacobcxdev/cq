package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/auth"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/provider"
	zaiprov "github.com/jacobcxdev/cq/internal/provider/zai"
	"github.com/jacobcxdev/cq/internal/provider/zai/zcode"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

// Pinned to the inspected ZCode protocol, independently of CQ's release version.
const zcodeClientVersion = zcode.ClientVersion

type ZAIZCodeCmd struct {
	Status ZAIZCodeStatusCmd `cmd:"" help:"Read token-plan balances and reset-card status"`
	Resets ZAIZCodeResetsCmd `cmd:"" help:"Explicitly request or use ZCode reset cards"`
	Claims ZAIZCodeClaimsCmd `cmd:"" help:"Inspect and claim currently offered token plans"`
}

type ZAIZCodeStatusCmd struct {
	Reference string `arg:"" optional:"" help:"OAuth account ID or unique label; omit for preferred login"`
}

type zcodeCommandDependencies struct {
	Accounts   func() ([]zaiprov.Account, error)
	Client     zcode.Client
	Out        io.Writer
	Resets     zcodeResetDependencies
	Claims     zcodeClaimsDependencies
	Invalidate func()
}

func defaultZCodeCommandDependencies() zcodeCommandDependencies {
	fsys := fsutil.OSFileSystem{}
	accounts := func() ([]zaiprov.Account, error) {
		roots, err := userdirs.Default()
		if err != nil {
			return nil, errors.New("resolve ZCode account directory")
		}
		return zaiprov.ReadAccounts(fsys, os.Getenv, roots.Config)
	}
	stateDir := func(name string) func() (string, error) {
		return func() (string, error) {
			roots, err := userdirs.Default()
			if err != nil {
				return "", errors.New("resolve ZCode state directory")
			}
			return filepath.Join(roots.State, name), nil
		}
	}
	client := zcode.Client{HTTP: zaiHTTPClient(15 * time.Second), Version: zcodeClientVersion}
	return zcodeCommandDependencies{
		Accounts: accounts, Client: client, Out: os.Stdout,
		Resets:     zcodeResetDependencies{Accounts: accounts, Client: client, FS: fsys, StateDir: stateDir("zai-reset-attempts"), In: os.Stdin, Out: os.Stdout, ErrOut: os.Stderr, Now: time.Now, RequestID: newZCodeCommandUUID},
		Claims:     zcodeClaimsDependencies{Accounts: accounts, Client: client, FS: fsys, StateDir: stateDir("zcode-claim-attempts"), In: os.Stdin, Out: os.Stdout, ErrOut: os.Stderr, Now: time.Now, OpenURL: openZCodeBrowser},
		Invalidate: func() { invalidateProviderCache(provider.ZAI) },
	}
}

func runZAIOAuthLogin(ctx context.Context, options ZAILoginCmd, dir string, jsonOutput bool, deps zaiCommandDependencies) error {
	if deps.deviceMid == nil || deps.oauthLogin == nil || deps.output == nil {
		return errors.New("ZCode browser login unavailable")
	}
	device, err := deps.deviceMid()
	if err != nil {
		return err
	}
	client := zcode.OAuthClient{HTTP: deps.client, Version: zcodeClientVersion, DeviceMid: device, OpenURL: func(ctx context.Context, target string) error {
		if deps.errOutput != nil {
			if _, err := fmt.Fprintln(deps.errOutput, "Complete ZCode sign-in in your browser."); err != nil {
				return errors.New("write ZCode sign-in guidance")
			}
		}
		if deps.openURL == nil {
			return errors.New("ZCode browser opener unavailable")
		}
		return deps.openURL(ctx, target)
	}}
	session, err := deps.oauthLogin(ctx, client)
	if err != nil {
		return err
	}
	session.DeviceMid = device
	account, err := zaiprov.StoreZCodeAccount(deps.fs, dir, session, options.Label, options.Activate)
	if err != nil {
		return err
	}
	if deps.invalidate != nil {
		deps.invalidate()
	}
	if jsonOutput {
		return json.NewEncoder(deps.output).Encode(account)
	}
	_, err = fmt.Fprintf(deps.output, "Saved ZCode login %s for Z.ai account %s.\n", session.Email, account.ID)
	return err
}

func runZAIZCodeCommand(ctx context.Context, command string, options ZAIZCodeCmd, jsonOutput bool, deps zcodeCommandDependencies) error {
	if strings.HasPrefix(command, "resets ") {
		subcommand := strings.TrimPrefix(command, "resets ")
		err := runZAIZCodeResetsCommand(ctx, subcommand, options.Resets, jsonOutput, deps.Resets)
		if err == nil && strings.HasPrefix(subcommand, "use ") && deps.Invalidate != nil {
			deps.Invalidate()
		}
		return err
	}
	if strings.HasPrefix(command, "claims ") {
		subcommand := strings.TrimPrefix(command, "claims ")
		err := runZAIZCodeClaimsCommand(ctx, subcommand, options.Claims, jsonOutput, deps.Claims)
		if err == nil && strings.HasPrefix(subcommand, "claim ") && deps.Invalidate != nil {
			deps.Invalidate()
		}
		return err
	}
	if command != "status" && command != "status <reference>" {
		return errors.New("unknown ZCode command")
	}
	if deps.Accounts == nil || deps.Out == nil {
		return errors.New("ZCode status command unavailable")
	}
	reference := options.Status.Reference
	if reference == "" {
		accounts, err := deps.Accounts()
		if err != nil {
			return err
		}
		for _, account := range accounts {
			if account.OAuth != nil {
				reference = account.ID
				break
			}
		}
	}
	selected, err := selectZCodeResetAccount(deps.Accounts, reference)
	if err != nil {
		return err
	}
	client := deps.Client
	client.Version, client.DeviceMid = zcodeClientVersion, selected.OAuth.DeviceMid
	if client.DeviceMid == "" {
		return errors.New("ZCode login has no device identity; run cq zai login --zcode again")
	}
	session := selected.OAuth.AsSession()
	balance, balanceErr := client.Balance(ctx, session)
	reset, resetErr := client.ResetStatus(ctx, session)
	result := struct {
		AccountID    string            `json:"account_id"`
		Label        string            `json:"label,omitempty"`
		Balance      zcode.Balance     `json:"balance"`
		Resets       zcode.ResetStatus `json:"resets"`
		BalanceError string            `json:"balance_error,omitempty"`
		ResetError   string            `json:"reset_error,omitempty"`
	}{AccountID: selected.ID, Label: selected.Label, Balance: balance, Resets: reset}
	if balanceErr != nil {
		result.BalanceError = balanceErr.Error()
	}
	if resetErr != nil {
		result.ResetError = resetErr.Error()
	}
	if jsonOutput {
		if err := json.NewEncoder(deps.Out).Encode(result); err != nil {
			return errors.New("write ZCode status")
		}
	} else {
		if _, err := fmt.Fprintf(deps.Out, "%s %s\n", selected.ID, selected.Label); err != nil {
			return errors.New("write ZCode status")
		}
		for _, bucket := range balance.Balances {
			if _, err := fmt.Fprintf(deps.Out, "  %s  remaining=%s  expires=%s\n", bucket.ShowName, bucket.RemainingUnits, bucket.ExpiresAt); err != nil {
				return errors.New("write ZCode status")
			}
		}
		if _, err := fmt.Fprintf(deps.Out, "  Reset cards: %d five-hour, %d weekly\n", len(reset.AvailableFiveHourResets), len(reset.AvailableWeekResets)); err != nil {
			return errors.New("write ZCode status")
		}
	}
	return errors.Join(balanceErr, resetErr)
}

func openZCodeBrowser(ctx context.Context, target string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	u, err := url.Parse(target)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("invalid ZCode browser URL")
	}
	if runtime.GOOS == "darwin" {
		if err := exec.CommandContext(ctx, "open", target).Run(); err != nil {
			return errors.New("open ZCode browser")
		}
		return nil
	}
	if err := auth.OpenBrowserContext(ctx, target); err != nil {
		return errors.New("open ZCode browser")
	}
	return nil
}

func newZCodeCommandUUID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return ""
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", value[:4], value[4:6], value[6:8], value[8:10], value[10:])
}

func zcodeDeviceMid() (string, error) {
	roots, err := userdirs.Default()
	if err != nil {
		return "", errors.New("resolve ZCode device directory")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("resolve ZCode device identity")
	}
	base := strings.TrimSpace(os.Getenv("ZCODE_DATA_BASE_DIR"))
	if base == "" {
		base = home
	}
	if base == "~" {
		base = home
	} else if strings.HasPrefix(base, "~/") {
		base = filepath.Join(home, base[2:])
	}
	return ensureZCodeCommandDevice(fsutil.OSFileSystem{}, roots.Config, filepath.Join(base, ".zcode", "v2", "telemetry-state.json"))
}

// The inspected official CLI reuses deviceMid or generates createUuid() under a
// local lock. CQ keeps its own registration without modifying ZCode-owned files.
func ensureZCodeCommandDevice(fsys fsutil.FileSystem, configDir, installedState string) (string, error) {
	path := filepath.Join(configDir, "zcode-device.json")
	read := func(name string) (string, error) {
		data, err := fsys.ReadFile(name)
		if err != nil {
			return "", err
		}
		var state struct {
			DeviceMid string `json:"deviceMid"`
		}
		if len(data) > 1<<20 || json.Unmarshal(data, &state) != nil || !zcodeDeviceHeader(state.DeviceMid) {
			return "", errors.New("invalid ZCode device identity")
		}
		return state.DeviceMid, nil
	}
	if existing, err := read(path); err == nil {
		return existing, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("read CQ ZCode device identity")
	}
	if err := fsutil.EnsureSecureDirectory(fsys, configDir); err != nil {
		return "", errors.New("prepare private ZCode device directory")
	}
	lock, err := fsutil.AcquireExclusiveLock(fsys, path+".lock")
	if err != nil {
		return "", errors.New("lock ZCode device identity")
	}
	defer lock.Close()
	if existing, err := read(path); err == nil {
		return existing, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("read CQ ZCode device identity")
	}
	device, err := read(installedState)
	if errors.Is(err, os.ErrNotExist) {
		device = newZCodeCommandUUID()
		if device == "" {
			return "", errors.New("create ZCode device identity")
		}
	} else if err != nil {
		return "", errors.New("read installed ZCode device identity")
	}
	data, _ := json.Marshal(struct {
		DeviceMid string `json:"deviceMid"`
	}{device})
	if err := fsutil.SecureAtomicWrite(fsys, path, append(data, '\n')); err != nil {
		return "", errors.New("save ZCode device identity")
	}
	return device, nil
}

func zcodeDeviceHeader(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for _, r := range value {
		if r <= ' ' || r >= 127 {
			return false
		}
	}
	return true
}
