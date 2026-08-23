package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/proxy"
)

const codexStopHookInputMax = 16 << 20

var (
	codexStopHookPoolPattern        = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	codexStopHookAccountHintPattern = regexp.MustCompile(`^codex:[0-9a-f]{12}$`)
)

type proxyCodexHookDependencies struct {
	LoadConfig func() (*proxy.Config, error)
	Doer       httputil.Doer
}

func runProxyHook(args []string, input io.Reader, output io.Writer) error {
	if len(args) != 1 || args[0] != "codex-stop" {
		return errors.New("usage: cq proxy hook codex-stop")
	}
	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("Codex turn receipt redirect refused")
		},
	}
	return runProxyCodexStopHook(context.Background(), input, output, proxyCodexHookDependencies{
		LoadConfig: proxy.LoadExistingConfig,
		Doer:       client,
	})
}

func runProxyCodexStopHook(ctx context.Context, input io.Reader, output io.Writer, deps proxyCodexHookDependencies) error {
	if ctx == nil || input == nil || output == nil || deps.LoadConfig == nil || deps.Doer == nil {
		return errors.New("Codex Stop hook dependencies unavailable")
	}
	body, err := io.ReadAll(io.LimitReader(input, codexStopHookInputMax+1))
	if err != nil || len(body) > codexStopHookInputMax {
		clear(body)
		return errors.New("invalid Codex Stop hook input")
	}
	defer clear(body)
	var hook struct {
		HookEventName string `json:"hook_event_name"`
		SessionID     string `json:"session_id"`
		TurnID        string `json:"turn_id"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&hook); err != nil {
		return errors.New("invalid Codex Stop hook input")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("invalid Codex Stop hook input")
	}
	session := []byte(hook.SessionID)
	turn := []byte(hook.TurnID)
	hook.SessionID = ""
	hook.TurnID = ""
	defer clear(session)
	defer clear(turn)
	if hook.HookEventName != "Stop" || !validCodexStopHookID(session) || !validCodexStopHookID(turn) {
		return errors.New("invalid Codex Stop hook input")
	}

	cfg, err := deps.LoadConfig()
	if err != nil {
		return fmt.Errorf("load proxy config: %w", err)
	}
	if cfg == nil || cfg.LocalToken == "" {
		return errors.New("proxy configuration unavailable")
	}
	port := cfg.Port
	if port == 0 {
		port = proxy.DefaultPort
	}
	if port < 1 || port > 65535 {
		return errors.New("proxy configuration unavailable")
	}
	payload, err := json.Marshal(struct {
		SessionID string `json:"session_id"`
		TurnID    string `json:"turn_id"`
	}{SessionID: string(session), TurnID: string(turn)})
	if err != nil {
		return errors.New("encode Codex turn receipt lookup")
	}
	defer clear(payload)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d%s", port, proxy.RuntimeCodexTurnReceiptV2Path), bytes.NewReader(payload))
	if err != nil {
		return errors.New("construct Codex turn receipt lookup")
	}
	request.Header.Set("Authorization", "Bearer "+cfg.LocalToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := deps.Doer.Do(request)
	if err != nil {
		return errors.New("Codex turn receipt lookup unavailable")
	}
	if response == nil || response.Body == nil {
		return errors.New("Codex turn receipt lookup unavailable")
	}
	defer response.Body.Close()
	responseBody, err := httputil.ReadBody(response.Body)
	if err != nil {
		return errors.New("Codex turn receipt lookup unavailable")
	}
	defer clear(responseBody)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Codex turn receipt lookup returned status %d", response.StatusCode)
	}
	var lookup proxy.CodexTurnReceiptLookupV2
	decoder = json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&lookup); err != nil {
		return errors.New("invalid Codex turn receipt response")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) || !validCodexTurnReceiptLookup(lookup) {
		return errors.New("invalid Codex turn receipt response")
	}
	if !lookup.Found {
		return json.NewEncoder(output).Encode(struct{}{})
	}
	return json.NewEncoder(output).Encode(struct {
		SystemMessage string `json:"systemMessage"`
	}{SystemMessage: formatCodexTurnReceipt(*lookup.Receipt)})
}

func validCodexStopHookID(value []byte) bool {
	if len(value) == 0 || len(value) > 4096 || !utf8.Valid(value) {
		return false
	}
	for _, current := range value {
		if current < 0x20 || current == 0x7f {
			return false
		}
	}
	return true
}

func validCodexTurnReceiptLookup(lookup proxy.CodexTurnReceiptLookupV2) bool {
	if lookup.SchemaVersion != 2 || lookup.Found != (lookup.Receipt != nil) {
		return false
	}
	if !lookup.Found {
		return true
	}
	receipt := *lookup.Receipt
	switch receipt.State {
	case proxy.CodexTurnReceiptPlanned, proxy.CodexTurnReceiptAttempted, proxy.CodexTurnReceiptCompleted, proxy.CodexTurnReceiptFailed, proxy.CodexTurnReceiptRejected, proxy.CodexTurnReceiptIndeterminate:
	default:
		return false
	}
	if receipt.Transport != proxy.CodexTurnReceiptTransportHTTP && receipt.Transport != proxy.CodexTurnReceiptTransportWebSocket {
		return false
	}
	if receipt.RequestKind != "turn" || receipt.CompactionPhase != "not_applicable" {
		return false
	}
	switch receipt.RequestLineage {
	case "previous_response_id_absent", "previous_response_id_present", "unknown":
	default:
		return false
	}
	switch receipt.RequestedModelClass {
	case "gpt_5_6_sol", "gpt_5_6_terra", "gpt_5_6_luna", "other", "unknown":
	default:
		return false
	}
	switch receipt.RequestedReasoningEffort {
	case "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra", "unspecified", "unknown":
	default:
		return false
	}
	if receipt.Pool != "" && !codexStopHookPoolPattern.MatchString(receipt.Pool) {
		return false
	}
	if receipt.PlannedAccountHint != "" && !codexStopHookAccountHintPattern.MatchString(receipt.PlannedAccountHint) {
		return false
	}
	if receipt.ActualAccountHint != "" && !codexStopHookAccountHintPattern.MatchString(receipt.ActualAccountHint) {
		return false
	}
	switch receipt.ShadowComparison {
	case proxy.CodexTurnReceiptShadowAlternativeAccount:
		if !codexStopHookAccountHintPattern.MatchString(receipt.ShadowAlternativeAccountHint) {
			return false
		}
	case proxy.CodexTurnReceiptShadowSameAccount, proxy.CodexTurnReceiptShadowNotApplicable, proxy.CodexTurnReceiptShadowUnavailable:
		if receipt.ShadowAlternativeAccountHint != "" {
			return false
		}
	default:
		return false
	}
	switch receipt.RouteReason {
	case proxy.CodexTurnReceiptRouteBound, proxy.CodexTurnReceiptRouteAffinityReuse, proxy.CodexTurnReceiptRouteFairnessSelect, proxy.CodexTurnReceiptRouteTerminalDefault, proxy.CodexTurnReceiptRouteUnknown:
		return true
	default:
		return false
	}
}

func formatCodexTurnReceipt(receipt proxy.CodexTurnReceiptV2) string {
	transport := "HTTP"
	if receipt.Transport == proxy.CodexTurnReceiptTransportWebSocket {
		transport = "WebSocket"
	}
	segments := []string{fmt.Sprintf("CQ route: %s via %s", receipt.State, transport)}
	if receipt.Pool != "" {
		segments = append(segments, "pool "+receipt.Pool)
	}
	accountHint := receipt.ActualAccountHint
	accountKind := "actual"
	if accountHint == "" {
		accountHint = receipt.PlannedAccountHint
		accountKind = "planned"
	}
	if accountHint != "" {
		segments = append(segments, fmt.Sprintf("account %s (%s)", accountHint, accountKind))
	}
	segments = append(segments, codexTurnReceiptModelName(receipt.RequestedModelClass)+"/"+codexTurnReceiptEffortName(receipt.RequestedReasoningEffort))
	segments = append(segments, codexTurnReceiptReason(receipt.RouteReason))
	message := strings.Join(segments, "; ") + ". Shadow: no-affinity comparison "
	switch receipt.ShadowComparison {
	case proxy.CodexTurnReceiptShadowSameAccount:
		return message + "agreed."
	case proxy.CodexTurnReceiptShadowAlternativeAccount:
		return message + "favoured account " + receipt.ShadowAlternativeAccountHint + "."
	case proxy.CodexTurnReceiptShadowNotApplicable:
		return message + "not applicable."
	default:
		return message + "unavailable."
	}
}

func codexTurnReceiptModelName(model string) string {
	switch model {
	case "gpt_5_6_sol":
		return "Sol"
	case "gpt_5_6_terra":
		return "Terra"
	case "gpt_5_6_luna":
		return "Luna"
	case "other":
		return "Other"
	default:
		return "Unknown"
	}
}

func codexTurnReceiptEffortName(effort string) string {
	switch effort {
	case "none":
		return "None"
	case "minimal":
		return "Minimal"
	case "low":
		return "Low"
	case "medium":
		return "Medium"
	case "high":
		return "High"
	case "xhigh":
		return "XHigh"
	case "max":
		return "Max"
	case "ultra":
		return "Ultra"
	case "unspecified":
		return "Unspecified"
	default:
		return "Unknown"
	}
}

func codexTurnReceiptReason(reason proxy.CodexTurnReceiptRouteReason) string {
	switch reason {
	case proxy.CodexTurnReceiptRouteBound:
		return "fixed account"
	case proxy.CodexTurnReceiptRouteAffinityReuse:
		return "warm affinity"
	case proxy.CodexTurnReceiptRouteFairnessSelect:
		return "quota/fairness"
	case proxy.CodexTurnReceiptRouteTerminalDefault:
		return "fallback"
	default:
		return "selected route"
	}
}
