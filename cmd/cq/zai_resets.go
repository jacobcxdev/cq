package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/provider"
	zaiprov "github.com/jacobcxdev/cq/internal/provider/zai"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

type ZAIResetsCmd struct {
	List     ZAIResetsListCmd    `cmd:"" help:"List personal Coding Plan reset cards"`
	Use      ZAIResetsUseCmd     `cmd:"" help:"Use one exact reset card after confirmation"`
	Schedule ZAIResetScheduleCmd `cmd:"" help:"Manage opt-in scheduled reset cards"`
}

type ZAIResetsListCmd struct {
	Reference string `arg:"" optional:"" help:"Account ID or unique label; omit for every account"`
}

type ZAIResetsUseCmd struct {
	Reference string `arg:"" help:"Account ID or unique label"`
	Credit    string `help:"Exact card ID from cq zai resets list"`
	Window    string `default:"5h" enum:"5h,7d" help:"Quota to reset; a weekly card also restores 5-hour quota"`
	Yes       bool   `help:"Consume the selected card without interactive confirmation"`
}

type zaiResetsDependencies struct {
	Accounts   func() ([]zaiprov.Account, error)
	Client     zaiprov.ResetClient
	FS         fsutil.FileSystem
	StateDir   func() (string, error)
	In         io.Reader
	Out        io.Writer
	ErrOut     io.Writer
	Now        func() time.Time
	RequestID  func() string
	Invalidate func()
}

func defaultZAIResetsDependencies() zaiResetsDependencies {
	return zaiResetsDependencies{
		Accounts: func() ([]zaiprov.Account, error) {
			roots, err := userdirs.Default()
			if err != nil {
				return nil, errors.New("resolve Z.ai credentials")
			}
			return zaiprov.ReadAccounts(fsutil.OSFileSystem{}, os.Getenv, roots.Config)
		},
		Client: zaiprov.ResetClient{HTTP: zaiHTTPClient(15 * time.Second)},
		FS:     fsutil.OSFileSystem{},
		StateDir: func() (string, error) {
			roots, err := userdirs.Default()
			return filepath.Join(roots.State, "zai-reset-attempts"), err
		},
		In: os.Stdin, Out: os.Stdout, ErrOut: os.Stderr, Now: time.Now,
		RequestID:  func() string { return "cq-zai-reset-" + rand.Text() },
		Invalidate: func() { invalidateProviderCache(provider.ZAI) },
	}
}

type zaiResetAccountInventory struct {
	AccountID string                `json:"account_id"`
	Label     string                `json:"label,omitempty"`
	Credits   []zaiprov.ResetCredit `json:"credits"`
	Error     string                `json:"error,omitempty"`
}

func runZAIResetsCommand(ctx context.Context, command string, cmd ZAIResetsCmd, jsonOutput bool, deps zaiResetsDependencies) error {
	accounts, err := deps.Accounts()
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		return errors.New("no Z.ai accounts; run cq zai login")
	}
	reference := cmd.List.Reference
	if strings.HasPrefix(command, "use ") || command == "use" {
		reference = cmd.Use.Reference
		if reference == "" {
			return errors.New("Z.ai reset account is required")
		}
	}
	if reference != "" {
		var selected []zaiprov.Account
		for _, account := range accounts {
			if account.ID == reference {
				selected = []zaiprov.Account{account}
				break
			}
			if account.Label == reference {
				selected = append(selected, account)
			}
		}
		if len(selected) != 1 {
			return errors.New("Z.ai account not found or label ambiguous; use its account ID")
		}
		accounts = selected
	}
	if strings.HasPrefix(command, "use ") || command == "use" {
		return runZAIResetUse(ctx, accounts[0], cmd.Use, jsonOutput, deps)
	}
	if command != "list" && command != "list <reference>" {
		return errors.New("unknown Z.ai reset command")
	}
	result := make([]zaiResetAccountInventory, 0, len(accounts))
	var failures []error
	for _, account := range accounts {
		inventory, err := deps.Client.ListAccount(ctx, &account, deps.Now())
		item := zaiResetAccountInventory{AccountID: account.ID, Label: account.Label, Credits: inventory.Credits}
		if err != nil {
			item.Error = err.Error()
			failures = append(failures, err)
		}
		result = append(result, item)
	}
	if jsonOutput {
		if err := json.NewEncoder(deps.Out).Encode(result); err != nil {
			return err
		}
	} else {
		for _, item := range result {
			if _, err := fmt.Fprintf(deps.Out, "%s %s\n", item.AccountID, item.Label); err != nil {
				return err
			}
			if item.Error != "" {
				if _, err := fmt.Fprintf(deps.Out, "  %s\n", item.Error); err != nil {
					return err
				}
			} else if len(item.Credits) == 0 {
				if _, err := fmt.Fprintln(deps.Out, "  No reset cards."); err != nil {
					return err
				}
			}
			for _, credit := range item.Credits {
				if _, err := fmt.Fprintf(deps.Out, "  %s  %s  available=%t  expires=%s\n", credit.ID, credit.Window, credit.Available, credit.ExpireTime); err != nil {
					return err
				}
			}
		}
	}
	return errors.Join(failures...)
}

func runZAIResetUse(ctx context.Context, account zaiprov.Account, cmd ZAIResetsUseCmd, jsonOutput bool, deps zaiResetsDependencies) error {
	dir, err := deps.StateDir()
	if err != nil {
		return errors.New("resolve Z.ai reset attempt directory")
	}
	executor := zaiprov.ResetExecutor{Client: deps.Client, FS: deps.FS, StateDir: dir, Now: deps.Now, RequestID: deps.RequestID}
	result, err := executor.Use(ctx, account, cmd.Window, cmd.Credit, func(credit zaiprov.ResetCredit) (bool, error) {
		if cmd.Yes {
			return true, nil
		}
		if _, err := fmt.Fprintf(deps.ErrOut, "Consume %s reset card %s for %s and restart its reset timer? A weekly reset also restores 5-hour quota. [y/N] ", credit.Window, credit.ID, account.ID); err != nil {
			return false, err
		}
		answer, err := bufio.NewReader(io.LimitReader(deps.In, 32)).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return false, errors.New("read Z.ai reset confirmation")
		}
		answer = strings.ToLower(strings.TrimSpace(answer))
		return answer == "y" || answer == "yes", nil
	})
	if err != nil {
		return err
	}
	deps.Invalidate()
	if jsonOutput {
		return json.NewEncoder(deps.Out).Encode(result)
	}
	if result.AlreadyAccepted {
		_, err = fmt.Fprintf(deps.Out, "Z.ai reset card %s was already accepted; no additional card sent.\n", result.CreditID)
	} else {
		_, err = fmt.Fprintf(deps.Out, "Z.ai accepted %s reset card %s for %s.\n", result.Window, result.CreditID, result.AccountID)
	}
	return err
}
