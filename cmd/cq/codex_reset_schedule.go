package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

// Local controls remain available even when credential discovery is broken.
func localCodexResetScheduleDependencies() (codexResetsDependencies, error) {
	roots, err := userdirs.Default()
	if err != nil {
		return codexResetsDependencies{}, err
	}
	return codexResetsDependencies{App: &app.CodexResetApp{Schedule: codex.NewResetAutomationStore(fsutil.OSFileSystem{}, filepath.Join(roots.State, "codex-reset-schedule.json")), Clock: systemClock{}}, In: os.Stdin, Out: os.Stdout, ErrOut: os.Stderr, ExecutorStatus: codexResetExecutorStatus}, nil
}

// Public output deliberately omits the private credential-routing AccountKey.
type codexAutomationEntryJSON struct {
	ID            string                          `json:"id"`
	AccountID     string                          `json:"account_id,omitempty"`
	Email         string                          `json:"email,omitempty"`
	CreditID      string                          `json:"credit_id"`
	At            *time.Time                      `json:"scheduled_at,omitempty"`
	ExpiresAt     *time.Time                      `json:"expires_at,omitempty"`
	UseBy         *time.Time                      `json:"use_by,omitempty"`
	Pinned        bool                            `json:"pinned"`
	Source        string                          `json:"source,omitempty"`
	Confidence    string                          `json:"confidence,omitempty"`
	State         codex.ResetAutomationEntryState `json:"state"`
	Outcome       codex.ConsumeResetOutcome       `json:"outcome,omitempty"`
	LastError     string                          `json:"last_error,omitempty"`
	Warnings      []string                        `json:"warnings,omitempty"`
	NextAttemptAt *time.Time                      `json:"next_attempt_at,omitempty"`
}
type codexAutomationJSON struct {
	Revision     uint64                     `json:"revision"`
	Enabled      bool                       `json:"enabled"`
	Mode         codex.ResetAutomationMode  `json:"mode"`
	PlanValid    bool                       `json:"plan_valid"`
	PlanError    string                     `json:"plan_error,omitempty"`
	LastReplanAt *time.Time                 `json:"last_replan_at,omitempty"`
	NextReplanAt *time.Time                 `json:"next_replan_at,omitempty"`
	Entries      []codexAutomationEntryJSON `json:"entries"`
	Executor     any                        `json:"executor,omitempty"`
}

func publicCodexAutomation(state codex.ResetAutomationState) codexAutomationJSON {
	result := codexAutomationJSON{Revision: state.Revision, Enabled: state.Enabled, Mode: state.Mode, PlanValid: state.PlanValid, PlanError: state.PlanError, LastReplanAt: state.LastReplanAt, NextReplanAt: state.NextReplanAt, Entries: []codexAutomationEntryJSON{}}
	for _, entry := range state.Entries {
		result.Entries = append(result.Entries, codexAutomationEntryJSON{ID: entry.ID, AccountID: entry.AccountID, Email: entry.Email, CreditID: entry.CreditID, At: entry.At, ExpiresAt: entry.ExpiresAt, UseBy: entry.UseBy, Pinned: entry.Pinned, Source: entry.Source, Confidence: entry.Confidence, State: entry.State, Outcome: entry.Outcome, LastError: entry.LastError, Warnings: entry.Warnings, NextAttemptAt: entry.NextAttemptAt})
	}
	return result
}
func runCodexResetsRecommendOptions(ctx context.Context, cmd CodexResetsRecommendCmd, jsonOutput bool, deps codexResetsDependencies) error {
	if !cmd.Schedule {
		if cmd.Mode != nil || cmd.Replace || cmd.Yes {
			return errors.New("--mode, --replace and --yes require --schedule")
		}
		return runCodexResetsRecommend(ctx, jsonOutput, deps)
	}
	mode := codex.ResetAutomationMode("")
	if cmd.Mode != nil {
		mode = codex.ResetAutomationMode(*cmd.Mode)
	}
	return runCodexResetScheduleChange(ctx, app.CodexResetScheduleChange{Action: "accept", Mode: mode, Replace: cmd.Replace}, cmd.Yes, jsonOutput, deps)
}
func runCodexResetScheduleCommand(ctx context.Context, command string, cmd CodexResetScheduleCmd, jsonOutput bool, deps codexResetsDependencies) error {
	if command == "codex resets schedule status" {
		return runCodexResetScheduleStatus(ctx, jsonOutput, deps)
	}
	change := app.CodexResetScheduleChange{}
	yes := false
	switch command {
	case "codex resets schedule add <account-reference>":
		change.Action = "add"
		change.Reference = cmd.Add.Reference
		yes = cmd.Add.Yes
		at, err := time.Parse(time.RFC3339, cmd.Add.At)
		if err != nil {
			return errors.New("--at must be an RFC3339 timestamp with timezone")
		}
		change.At = &at
		if cmd.Add.Credit != nil {
			change.CreditID = strings.TrimSpace(*cmd.Add.Credit)
			if change.CreditID == "" {
				return errors.New("credit ID is empty")
			}
		}
	case "codex resets schedule set <entry>":
		change.Action = "set"
		change.EntryID = cmd.Set.Entry
		change.FollowMode = cmd.Set.FollowMode
		yes = cmd.Set.Yes
		if cmd.Set.At != "" {
			at, err := time.Parse(time.RFC3339, cmd.Set.At)
			if err != nil {
				return errors.New("--at must be an RFC3339 timestamp with timezone")
			}
			change.At = &at
		}
	case "codex resets schedule mode <value>":
		change.Action = "mode"
		change.Mode = codex.ResetAutomationMode(cmd.Mode.Value)
		yes = cmd.Mode.Yes
	case "codex resets schedule remove <entry>":
		change.Action = "remove"
		change.EntryID = cmd.Remove.Entry
	case "codex resets schedule enable":
		change.Action = "enable"
		yes = cmd.Enable.Yes
	case "codex resets schedule disable":
		change.Action = "disable"
	case "codex resets schedule cancel":
		change.Action = "cancel"
	default:
		return errors.New("unknown reset schedule command")
	}
	return runCodexResetScheduleChange(ctx, change, yes, jsonOutput, deps)
}
func runCodexResetScheduleChange(ctx context.Context, change app.CodexResetScheduleChange, yes, jsonOutput bool, deps codexResetsDependencies) error {
	if err := validateCodexResetsDependencies(deps); err != nil {
		return err
	}
	plan, err := deps.App.PrepareSchedule(ctx, change)
	if err != nil {
		return err
	}
	if plan.ConfirmationRequired {
		preview := deps.Out
		if jsonOutput {
			preview = deps.ErrOut
		}
		if err := renderCodexAutomation(preview, plan.State); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(preview, "Saving authorises automatic execution while CQ proxy runs. Due entries may execute immediately."); err != nil {
			return err
		}
		if !yes {
			if deps.In == nil || !codexResetInputIsInteractive(deps.In) {
				return errors.New("non-interactive reset scheduling requires --yes")
			}
			if _, err := fmt.Fprint(deps.ErrOut, "Save this reset schedule? [y/N] "); err != nil {
				return err
			}
			accepted := false
			if deps.In != nil {
				line, _ := bufio.NewReader(deps.In).ReadString('\n')
				answer := strings.TrimSpace(line)
				accepted = strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes")
			}
			if !accepted {
				if jsonOutput {
					return encodeCodexResetJSON(deps.Out, struct {
						Cancelled bool `json:"cancelled"`
					}{true})
				}
				_, err := fmt.Fprintln(deps.Out, "Schedule unchanged.")
				return err
			}
		}
	}
	state, err := deps.App.ApplySchedule(ctx, plan)
	if err != nil {
		return err
	}
	if jsonOutput {
		return encodeCodexResetJSON(deps.Out, publicCodexAutomation(state))
	}
	return renderCodexAutomation(deps.Out, state)
}
func runCodexResetScheduleStatus(ctx context.Context, jsonOutput bool, deps codexResetsDependencies) error {
	if err := validateCodexResetsDependencies(deps); err != nil {
		return err
	}
	state, err := deps.App.Schedule.Load()
	if err != nil {
		return err
	}
	executor := proxy.CodexResetSchedulerStatus{State: "unavailable"}
	if deps.ExecutorStatus != nil {
		executor = deps.ExecutorStatus(ctx)
	}
	if jsonOutput {
		result := publicCodexAutomation(state)
		result.Executor = executor
		return encodeCodexResetJSON(deps.Out, result)
	}
	if err := renderCodexAutomation(deps.Out, state); err != nil {
		return err
	}
	_, err = fmt.Fprintf(deps.Out, "\nExecutor: %s\n", executor.State)
	return err
}
func renderCodexAutomation(w io.Writer, state codex.ResetAutomationState) error {
	if _, err := fmt.Fprintf(w, "Codex automated reset schedule\n  enabled: %t\n  mode: %s\n  revision: %d\n", state.Enabled, state.Mode, state.Revision); err != nil {
		return err
	}
	if state.PlanError != "" {
		if _, err := fmt.Fprintf(w, "  plan: %s\n", state.PlanError); err != nil {
			return err
		}
	}
	for _, entry := range state.Entries {
		pin := ""
		if entry.Pinned {
			pin = " pinned"
		}
		at := "not scheduled"
		if entry.At != nil {
			at = formatCodexResetTime(*entry.At)
		}
		if _, err := fmt.Fprintf(w, "\n%s  %s  %s\n  credit: %s\n  at: %s%s\n  expires: %s\n  confidence: %s\n", entry.ID, codexResetAccountLabel(entry.Email, entry.AccountID), entry.State, entry.CreditID, at, pin, formatCodexResetExpiry(entry.ExpiresAt), entry.Confidence); err != nil {
			return err
		}
		if entry.LastError != "" {
			if _, err := fmt.Fprintf(w, "  reason: %s\n", entry.LastError); err != nil {
				return err
			}
		}
		for _, warning := range entry.Warnings {
			if _, err := fmt.Fprintf(w, "  warning: %s\n", warning); err != nil {
				return err
			}
		}
	}
	if len(state.Entries) == 0 {
		_, err := fmt.Fprintln(w, "  no scheduled resets")
		return err
	}
	return nil
}
