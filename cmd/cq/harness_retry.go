package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/jacobcxdev/cq/internal/harness"
)

func (runner codexHarnessRunner) harnessRetry(ctx context.Context, cmd CodexHarnessExplainCmd, jsonOutput bool, output io.Writer) error {
	project, err := runner.harnessProject(cmd.Project)
	if err != nil {
		return err
	}
	chat, err := runner.harnessChat(cmd.Chat, true)
	if err != nil {
		return err
	}
	model := cmd.Model
	if model == "" {
		if runner.readModel == nil {
			return fmt.Errorf("current model unavailable; pass --model")
		}
		snapshot, err := runner.readModel(ctx, chat)
		if err != nil {
			return err
		}
		if snapshot.ChatID != chat || snapshot.Model == "" {
			return fmt.Errorf("current model response does not identify the selected chat")
		}
		model = snapshot.Model
	}
	if runner.receipt == nil {
		return fmt.Errorf("harness application receipt unavailable")
	}
	policy, _, err := runner.harnessContext(project)
	if err != nil {
		return err
	}
	check := func(policy harness.Policy, receipt harness.Receipt) error {
		if receipt.Mode == "full" && !receipt.RetrySupported {
			return fmt.Errorf("full-mode owner does not support retry; update the owner before requesting retry")
		}
		selection := policy.Resolve(model, project, chat)
		if policy.Version != 2 || !policy.Enabled || !selection.Managed || receipt.ChatID != chat || receipt.Mode != "full" || !receipt.RetrySupported || receipt.State != "failed" || receipt.Profile != selection.Profile || receipt.Fingerprint != selection.Fingerprint || receipt.Model != model || receipt.Cwd != project || receipt.UpdatedAt.IsZero() {
			return fmt.Errorf("retry requires a matching failed full-mode receipt for this chat, project and model")
		}
		return nil
	}
	receipt, err := runner.receipt(chat)
	if err != nil {
		return err
	}
	if err := check(policy, receipt); err != nil {
		return err
	}
	if runner.requestRetry == nil {
		return fmt.Errorf("native harness retry requests unavailable")
	}
	// The receipt store serialises intent independently of the native owner's lease.
	_, err = runner.requestRetry(chat, harness.RetryRequest{Fingerprint: receipt.Fingerprint, Model: model, Cwd: project, FailedAt: receipt.UpdatedAt})
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(output).Encode(map[string]any{"saved": true, "chat": chat, "project": project, "model": model, "application": "requested", "application_reason": "next_idle_boundary"})
	}
	_, err = fmt.Fprintf(output, "Harness retry requested for %s at its next idle boundary. Inspect explain for application proof.\n", chat)
	return err
}
