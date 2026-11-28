package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/app"
	"github.com/jacobcxdev/cq/internal/fsutil"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/quota"
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
	AutoEnrol    bool                       `json:"auto_enrol"`
	PlanValid    bool                       `json:"plan_valid"`
	PlanError    string                     `json:"plan_error,omitempty"`
	LastReplanAt *time.Time                 `json:"last_replan_at,omitempty"`
	NextReplanAt *time.Time                 `json:"next_replan_at,omitempty"`
	NextPace     *quota.ResetPacePhase      `json:"next_pace,omitempty"`
	Entries      []codexAutomationEntryJSON `json:"entries"`
	Executor     any                        `json:"executor,omitempty"`
}

func publicCodexAutomation(state codex.ResetAutomationState) codexAutomationJSON {
	result := codexAutomationJSON{Revision: state.Revision, Enabled: state.Enabled, Mode: state.Mode, AutoEnrol: state.AutoEnrol, PlanValid: state.PlanValid, PlanError: state.PlanError, LastReplanAt: state.LastReplanAt, NextReplanAt: state.NextReplanAt, Entries: []codexAutomationEntryJSON{}}
	if state.Enabled && state.PlanValid && state.Mode == codex.ResetAutomationAdaptive {
		result.NextPace = state.NextPace
	}
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
		if _, err := fmt.Fprintln(preview, "Schedule preview"); err != nil {
			return err
		}
		if err := renderCodexAutomation(preview, plan.State, nil); err != nil {
			return err
		}
		authority := "The schedule will stay paused until enabled."
		if plan.State.Enabled {
			if plan.State.AutoEnrol {
				authority = "Saving authorises CQ to enrol and redeem eligible current and future credits while the proxy runs."
			} else {
				authority = "Saving authorises CQ to redeem the scheduled credits while the proxy runs."
			}
			authority += " Due entries may execute immediately."
		}
		if _, err := fmt.Fprintln(preview, "\n"+authority); err != nil {
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
	return renderCodexAutomation(deps.Out, state, nil)
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
	if state.NextPace != nil {
		now := time.Now()
		if deps.App.Clock != nil {
			now = deps.App.Clock.Now()
		}
		if !executor.Running || executor.LastError != "" || !state.FreshAdaptivePlan(now) || !state.NextPace.Until.After(now) {
			state.NextPace = nil
		}
	}
	if jsonOutput {
		result := publicCodexAutomation(state)
		result.Executor = executor
		return encodeCodexResetJSON(deps.Out, result)
	}
	return renderCodexAutomation(deps.Out, state, &executor)
}

func renderCodexAutomation(w io.Writer, state codex.ResetAutomationState, executor *proxy.CodexResetSchedulerStatus) error {
	var out strings.Builder
	mode := "Fixed"
	if state.Mode == codex.ResetAutomationAdaptive {
		mode = "Adaptive"
	}
	status := "Paused"
	if state.Enabled {
		status = "Enabled"
	} else if state.Revision == 0 {
		status = "Off"
	}
	fmt.Fprintf(&out, "Codex reset schedule · %s · %s\n", mode, status)
	if executor != nil {
		fmt.Fprintf(&out, "  Executor %s\n", strings.ReplaceAll(executor.State, "_", " "))
		if executor.LastError != "" {
			fmt.Fprintf(&out, "  Executor reported %s.\n", strings.ReplaceAll(executor.LastError, "_", " "))
		}
	}
	if state.Enabled && state.Mode == codex.ResetAutomationAdaptive {
		if state.AutoEnrol {
			fmt.Fprintln(&out, "  New credits are picked up automatically.")
		} else {
			fmt.Fprintln(&out, "  New credits require approval. To allow auto-enrolment, run cq codex resets schedule mode adaptive.")
		}
		if state.LastReplanAt != nil {
			fmt.Fprintf(&out, "  Last checked %s\n", formatCodexResetDisplayTime(*state.LastReplanAt))
		}
		if state.NextReplanAt != nil {
			fmt.Fprintf(&out, "  Next check %s\n", formatCodexResetDisplayTime(*state.NextReplanAt))
		}
		if pace := state.NextPace; pace != nil && state.PlanValid && (executor == nil || executor.Running) {
			fmt.Fprintf(&out, "  Slow down to %d%% of observed burn\n    From %s until %s\n",
				pace.Percent, formatCodexResetDisplayTime(pace.From), formatCodexResetDisplayTime(pace.Until))
		}
	}
	if state.PlanError != "" {
		fmt.Fprintf(&out, "  %s\n", codexResetScheduleExplanation(state.PlanError))
	}

	var upcoming, history []codex.ResetAutomationEntry
	for _, entry := range state.Entries {
		if entry.Terminal() {
			history = append(history, entry)
		} else {
			upcoming = append(upcoming, entry)
		}
	}
	sort.SliceStable(upcoming, func(i, j int) bool {
		left, right := upcoming[i], upcoming[j]
		leftInFlight := left.State == codex.ResetAutomationRunning || left.State == codex.ResetAutomationRetrying
		rightInFlight := right.State == codex.ResetAutomationRunning || right.State == codex.ResetAutomationRetrying
		if leftInFlight != rightInFlight {
			return leftInFlight
		}
		if left.At != nil {
			return right.At == nil || left.At.Before(*right.At)
		}
		return right.At == nil && left.ExpiresAt != nil && (right.ExpiresAt == nil || left.ExpiresAt.Before(*right.ExpiresAt))
	})
	sort.SliceStable(history, func(i, j int) bool { return history[i].UpdatedAt.After(history[j].UpdatedAt) })

	fmt.Fprintf(&out, "\nUpcoming (%d)\n", len(upcoming))
	if len(upcoming) == 0 {
		fmt.Fprintln(&out, "  No pending resets.")
	}
	for _, entry := range upcoming {
		details := []string{codexResetEntryStatus(entry)}
		if entry.Pinned {
			details = append(details, "Pinned")
		}
		if entry.Source == "adaptive" {
			details = append(details, "Auto-enrolled")
		}
		if entry.Confidence == "low" && entry.At != nil && !entry.Pinned && entry.State == codex.ResetAutomationPending {
			details = append(details, "Low confidence")
		}
		fmt.Fprintf(&out, "  %s · %s\n", codexResetAccountLabel(entry.Email, entry.AccountID), strings.Join(details, " · "))
		var timing []string
		if entry.NextAttemptAt != nil {
			timing = append(timing, "Retry "+formatCodexResetDisplayTime(*entry.NextAttemptAt))
		} else if entry.At != nil {
			timing = append(timing, "Planned "+formatCodexResetDisplayTime(*entry.At))
		}
		if entry.ExpiresAt != nil {
			expires := "Expires " + formatCodexResetDisplayTime(*entry.ExpiresAt)
			if entry.At != nil && entry.State == codex.ResetAutomationPending {
				margin := entry.ExpiresAt.Sub(*entry.At)
				if margin > 0 && margin <= 5*time.Minute {
					if margin%time.Minute == 0 {
						expires += fmt.Sprintf(" (%dm before expiry)", margin/time.Minute)
					} else {
						expires += fmt.Sprintf(" (%ds before expiry)", margin/time.Second)
					}
				}
			}
			timing = append(timing, expires)
		}
		if len(timing) != 0 {
			fmt.Fprintf(&out, "    %s\n", strings.Join(timing, " · "))
		}
		fmt.Fprintf(&out, "    Entry %s\n", entry.ID)
	}
	if len(history) != 0 {
		fmt.Fprintf(&out, "\nHistory (%d)\n", len(history))
	}
	for _, entry := range history {
		at := entry.UpdatedAt
		if at.IsZero() && entry.At != nil {
			at = *entry.At
		}
		fmt.Fprintf(&out, "  %s · %s · %s\n", formatCodexResetDisplayTime(at), codexResetAccountLabel(entry.Email, entry.AccountID), codexResetEntryStatus(entry))
		for _, warning := range entry.Warnings {
			fmt.Fprintf(&out, "    %s\n", codexResetWarningMessage(warning))
		}
	}
	_, err := io.WriteString(w, out.String())
	return err
}

func codexResetEntryStatus(entry codex.ResetAutomationEntry) string {
	switch entry.State {
	case codex.ResetAutomationPending:
		if entry.LastError == "credit_unavailable" {
			return "Credit unavailable"
		}
		if entry.LastError == "quota_remaining" {
			return "Waiting for existing quota to be spent"
		}
		if entry.At == nil {
			return "Waiting for usage"
		}
		return "Scheduled"
	case codex.ResetAutomationRunning:
		return "Redeeming"
	case codex.ResetAutomationRetrying:
		return "Confirming previous attempt"
	case codex.ResetAutomationBlocked:
		return codexResetScheduleExplanation(entry.LastError)
	case codex.ResetAutomationCompleted:
		if entry.Outcome == codex.ConsumeAlreadyRedeemed {
			return "Already redeemed"
		}
		return "Reset succeeded"
	case codex.ResetAutomationSkipped:
		return "Skipped because quota was full"
	case codex.ResetAutomationMissed:
		if entry.LastError == "credit_expired" {
			return "Expired unused"
		}
		return "Credit unavailable"
	case codex.ResetAutomationCancelled:
		return "Cancelled"
	default:
		return string(entry.State)
	}
}

func codexResetScheduleExplanation(code string) string {
	switch code {
	case "recommendation_incomplete":
		return "Waiting for fresh quota and credit inventory"
	case "attempts_unavailable":
		return "Waiting for reset attempt storage"
	case "reserve_order_changed":
		return "Waiting for a plan that preserves the quota reserve"
	case "time_required":
		return "Needs a scheduled time"
	case "":
		return "Blocked"
	default:
		return "Blocked · " + strings.ReplaceAll(code, "_", " ")
	}
}
