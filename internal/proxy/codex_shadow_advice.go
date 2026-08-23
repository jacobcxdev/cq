package proxy

import (
	"context"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

type codexNoAffinityShadowResult struct {
	Comparison            CodexTurnReceiptShadowComparison
	AlternativeAccountKey codex.AccountKey
}

func codexNoAffinityShadowAdvice(ctx context.Context, dispatch CodexFrozenDispatchPlan, defaultAccountKey, boundAccountKey codex.AccountKey, actual CodexFrozenDispatchAccount) codexNoAffinityShadowResult {
	if boundAccountKey != "" || actual.decision != codexRuntimeDecisionAffinityReuse {
		return codexNoAffinityShadowResult{Comparison: CodexTurnReceiptShadowNotApplicable}
	}
	if len(dispatch.policyCandidates) == 0 {
		return codexNoAffinityShadowResult{Comparison: CodexTurnReceiptShadowUnavailable}
	}
	plan, err := BuildCodexRoutePlan(ctx, dispatch.policyCandidates, CodexRoutePolicyHints{
		DefaultAccountKey: defaultAccountKey,
	})
	if err != nil {
		return codexNoAffinityShadowResult{Comparison: CodexTurnReceiptShadowUnavailable}
	}
	choices := plan.Choices()
	if len(choices) == 0 {
		return codexNoAffinityShadowResult{Comparison: CodexTurnReceiptShadowUnavailable}
	}
	if choices[0].AccountKey == actual.choice.AccountKey {
		return codexNoAffinityShadowResult{Comparison: CodexTurnReceiptShadowSameAccount}
	}
	return codexNoAffinityShadowResult{
		Comparison:            CodexTurnReceiptShadowAlternativeAccount,
		AlternativeAccountKey: choices[0].AccountKey,
	}
}
