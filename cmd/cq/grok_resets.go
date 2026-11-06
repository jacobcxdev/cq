package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jacobcxdev/cq/internal/provider/grok"
)

func runGrokResetList(ctx context.Context, reference string, jsonOutput bool, authority grok.Authority, output io.Writer) error {
	accounts, err := authority.Accounts(ctx)
	if err != nil {
		return err
	}
	if reference != "" {
		selected := make([]grok.Account, 0, 1)
		for _, account := range accounts {
			if string(account.ID) == reference || account.Label == reference || account.Email == reference {
				selected = append(selected, account)
			}
		}
		if len(selected) != 1 {
			return errors.New("Grok account reference must match one account")
		}
		accounts = selected
	}
	type inventory struct {
		AccountID grok.AccountID     `json:"account_id"`
		Rewards   []grok.ResetReward `json:"rewards"`
	}
	results := make([]inventory, 0, len(accounts))
	for _, account := range accounts {
		rewards, err := grok.ListResets(ctx, authority, account.ID, time.Now())
		if err != nil {
			return err
		}
		results = append(results, inventory{account.ID, rewards})
	}
	if jsonOutput {
		return json.NewEncoder(output).Encode(results)
	}
	for _, item := range results {
		if _, err := fmt.Fprintf(output, "%s: %d reset reward(s)\n", item.AccountID, len(item.Rewards)); err != nil {
			return err
		}
		for _, reward := range item.Rewards {
			if _, err := fmt.Fprintf(output, "  Expires %s\n", reward.ExpiresAt.UTC().Format(time.RFC3339)); err != nil {
				return err
			}
		}
	}
	return nil
}
