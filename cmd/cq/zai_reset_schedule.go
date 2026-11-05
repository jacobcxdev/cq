package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	zaiprov "github.com/jacobcxdev/cq/internal/provider/zai"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

type ZAIResetScheduleCmd struct {
	Status struct{} `cmd:"" default:"1" help:"Show saved reset dates and execution outcomes"`
	Add    struct {
		Reference string `arg:"" help:"Account ID or unique label"`
		At        string `required:"" help:"Future RFC3339 timestamp with timezone"`
		Window    string `default:"5h" enum:"5h,7d" help:"Quota to reset; weekly resets also restore 5-hour quota"`
		Credit    string `help:"Exact card ID; defaults to earliest known expiry"`
		Yes       bool   `help:"Confirm adding to an already enabled schedule"`
	} `cmd:"" help:"Bind an exact reset card to a fixed future date"`
	Remove struct {
		Entry string `arg:"" help:"Pending schedule entry ID"`
	} `cmd:"" help:"Cancel one pending entry while retaining its history"`
	Enable struct {
		Yes bool `help:"Enable accepted entries without interactive confirmation"`
	} `cmd:"" help:"Enable scheduled card use after confirmation"`
	Disable struct{} `cmd:"" help:"Disable future scheduled execution"`
	Cancel  struct{} `cmd:"" help:"Disable execution and cancel pending entries"`
}

type zaiResetScheduleDependencies struct {
	Scheduler func() (*proxy.ZAIResetScheduler, error)
	Accounts  func() ([]zaiprov.Account, error)
	Client    zaiprov.ResetClient
	In        io.Reader
	Out       io.Writer
	ErrOut    io.Writer
	Now       func() time.Time
}

func defaultZAIResetScheduleDependencies() zaiResetScheduleDependencies {
	base := defaultZAIResetsDependencies()
	return zaiResetScheduleDependencies{
		Accounts: base.Accounts, Client: base.Client, In: base.In, Out: base.Out, ErrOut: base.ErrOut, Now: base.Now,
		Scheduler: func() (*proxy.ZAIResetScheduler, error) {
			roots, err := userdirs.Default()
			if err != nil {
				return nil, errors.New("resolve Z.ai reset schedule directory")
			}
			executor := zaiprov.ResetExecutor{Client: base.Client, FS: base.FS, StateDir: filepath.Join(roots.State, "zai-reset-attempts"), Now: base.Now, RequestID: base.RequestID}
			return proxy.NewZAIResetScheduler(base.FS, roots.State, base.Accounts, executor)
		},
	}
}

func runZAIResetScheduleCommand(ctx context.Context, command string, cmd ZAIResetScheduleCmd, jsonOutput bool, deps zaiResetScheduleDependencies) error {
	scheduler, err := deps.Scheduler()
	if err != nil {
		return err
	}
	state, err := scheduler.Store.Load()
	if err != nil {
		return err
	}
	switch command {
	case "status":
		return json.NewEncoder(deps.Out).Encode(scheduler.Health())
	case "add <reference>":
		at, err := time.Parse(time.RFC3339, cmd.Add.At)
		if err != nil || !at.After(deps.Now()) {
			return errors.New("--at must be a future RFC3339 timestamp with timezone")
		}
		accounts, err := deps.Accounts()
		if err != nil {
			return errors.New("read Z.ai schedule accounts")
		}
		account, err := zaiScheduleAccount(accounts, cmd.Add.Reference)
		if err != nil {
			return err
		}
		inventory, err := deps.Client.ListAccount(ctx, &account, deps.Now())
		if err != nil {
			return err
		}
		credit, err := zaiprov.SelectResetCredit(inventory, cmd.Add.Window, cmd.Add.Credit, deps.Now())
		if err != nil {
			return err
		}
		if credit.ExpiresAt != nil && !at.Before(*credit.ExpiresAt) {
			return errors.New("scheduled Z.ai reset must precede card expiry")
		}
		accepted := cmd.Add.Yes
		if state.Enabled && !accepted {
			accepted, err = confirmZAISchedule(deps, fmt.Sprintf("Schedule %s reset card %s for %s at %s? [y/N] ", credit.Window, credit.ID, account.ID, at.Format(time.RFC3339)))
			if err != nil {
				return err
			}
			if !accepted {
				return errors.New("Z.ai schedule change cancelled")
			}
		}
		state, err = scheduler.Store.Add(account.ID, credit, at, deps.Now(), accepted)
		if err != nil {
			return err
		}
	case "remove <entry>":
		state, err = scheduler.Store.Remove(cmd.Remove.Entry)
	case "enable":
		if !state.Enabled && !cmd.Enable.Yes {
			if err := printZAIResetSchedule(deps.Out, state); err != nil {
				return err
			}
			accepted, confirmErr := confirmZAISchedule(deps, "Enable Z.ai scheduled reset-card use? Due entries may consume their exact cards immediately. Weekly resets also restore 5-hour quota. [y/N] ")
			if confirmErr != nil {
				return confirmErr
			}
			if !accepted {
				return errors.New("Z.ai schedule enable cancelled")
			}
		}
		if !state.Enabled {
			state, err = scheduler.Store.EnableReviewed(state)
		}
	case "disable":
		state, err = scheduler.Store.SetEnabled(false)
	case "cancel":
		state, err = scheduler.Store.Cancel()
	default:
		return errors.New("unknown Z.ai reset schedule command")
	}
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(deps.Out).Encode(state)
	}
	return printZAIResetSchedule(deps.Out, state)
}

func printZAIResetSchedule(out io.Writer, state proxy.ZAIResetScheduleState) error {
	if _, err := fmt.Fprintf(out, "Z.ai reset schedule: enabled=%t, entries=%d. CQ checks accepted dates while running.\n", state.Enabled, len(state.Entries)); err != nil {
		return err
	}
	for _, entry := range state.Entries {
		if _, err := fmt.Fprintf(out, "  %s  %s  %s  %s  %s\n", entry.ID, entry.AccountID, entry.CreditID, entry.At.Format(time.RFC3339), entry.State); err != nil {
			return err
		}
	}
	return nil
}

func confirmZAISchedule(deps zaiResetScheduleDependencies, prompt string) (bool, error) {
	if _, err := fmt.Fprint(deps.ErrOut, prompt); err != nil {
		return false, err
	}
	answer, err := bufio.NewReader(io.LimitReader(deps.In, 32)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, errors.New("read Z.ai schedule confirmation")
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}

func zaiScheduleAccount(accounts []zaiprov.Account, reference string) (zaiprov.Account, error) {
	for _, account := range accounts {
		if account.ID == reference {
			return account, nil
		}
	}
	var match *zaiprov.Account
	for i := range accounts {
		if reference != "" && accounts[i].Label == reference {
			if match != nil {
				return zaiprov.Account{}, errors.New("Z.ai account label ambiguous; use its account ID")
			}
			match = &accounts[i]
		}
	}
	if match == nil {
		return zaiprov.Account{}, errors.New("Z.ai account not found")
	}
	return *match, nil
}
