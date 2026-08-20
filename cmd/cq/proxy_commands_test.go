package main

import (
	"strings"
	"testing"
	"time"
)

func TestProxyCommandClassifiesThirteenOrdinaryRows(t *testing.T) {
	tests := []struct {
		argv []string
		row  string
	}{
		{nil, "check"},
		{[]string{"check", "codex", "claude"}, "check"},
		{[]string{"claude", "login"}, "claude_login"},
		{[]string{"claude", "accounts"}, "claude_accounts"},
		{[]string{"claude", "switch", "a@example.com"}, "claude_switch"},
		{[]string{"claude", "remove", "a@example.com"}, "claude_remove"},
		{[]string{"codex", "login"}, "codex_login"},
		{[]string{"codex", "accounts"}, "codex_accounts"},
		{[]string{"codex", "switch", "a@example.com"}, "codex_switch"},
		{[]string{"codex", "remove", "a@example.com"}, "codex_remove"},
		{[]string{"gemini", "accounts"}, "gemini_accounts"},
		{[]string{"--help"}, "ordinary_help"},
		{[]string{"--version"}, "ordinary_version"},
		{[]string{"claude"}, "ordinary_usage_error"},
	}
	rows := map[string]bool{}
	for _, test := range tests {
		got, err := ClassifyProxyCommand(test.argv)
		if err != nil {
			t.Fatalf("ClassifyProxyCommand(%v): %v", test.argv, err)
		}
		if got.Row != test.row {
			t.Fatalf("ClassifyProxyCommand(%v).Row = %q, want %q", test.argv, got.Row, test.row)
		}
		rows[got.Row] = true
	}
	if len(rows) != 13 {
		t.Fatalf("distinct ordinary rows = %d, want 13: %v", len(rows), rows)
	}
}

func TestProxyCommandClassifiesRefreshAndOperatorRows(t *testing.T) {
	tests := []struct {
		argv      []string
		catalogue string
		row       string
	}{
		{[]string{"refresh"}, "refresh", "refresh"},
		{[]string{"agent", "install", "ignored"}, "refresh", "agent_install"},
		{[]string{"agent", "uninstall", "--later"}, "refresh", "agent_uninstall"},
		{[]string{"operation", "status", "--operation-id", strings.Repeat("a", 32), "--json"}, "operator_recovery", "operator_status"},
		{[]string{"operation", "recover", "--json", "--operation-id", strings.Repeat("a", 32)}, "operator_recovery", "operator_recover"},
	}
	for _, test := range tests {
		got, err := ClassifyProxyCommand(test.argv)
		if err != nil {
			t.Fatalf("ClassifyProxyCommand(%v): %v", test.argv, err)
		}
		if got.Catalogue != test.catalogue || got.Row != test.row {
			t.Fatalf("ClassifyProxyCommand(%v) = %s/%s, want %s/%s", test.argv, got.Catalogue, got.Row, test.catalogue, test.row)
		}
	}
	implicit := ImplicitEnsureAgentAuthority()
	if implicit.Catalogue != "refresh" || implicit.Row != "implicit_ensure_agent" {
		t.Fatalf("implicit authority = %+v", implicit)
	}
}

func TestProxyCommandHelpAnywhereAndIgnoredRefreshTails(t *testing.T) {
	for _, argv := range [][]string{
		{"help"},
		{"check", "codex", "help", "ignored"},
		{"agent", "install", "ignored", "--help", "later"},
		{"agent", "uninstall", "-h", "ignored"},
		{"agent", "install", "ignored", "help"},
		{"agent", "help", "install"},
		{"agent", "install", "--", "--help"},
	} {
		got, err := ClassifyProxyCommand(argv)
		if err != nil || got.Row != "ordinary_help" || !got.Terminating {
			t.Fatalf("ClassifyProxyCommand(%v) = %+v, %v; want terminating help", argv, got, err)
		}
	}
	got, err := ClassifyProxyCommand([]string{"agent", "unknown", "--help"})
	if err != nil || got.Row != "ordinary_usage_error" || !got.Terminating {
		t.Fatalf("unknown agent help = %+v, %v; want usage", got, err)
	}
	for _, argv := range [][]string{
		{"agent", "install", "one", "--unknown", "two"},
		{"agent", "uninstall", "one", "two"},
	} {
		got, err := ClassifyProxyCommand(argv)
		if err != nil || got.IgnoredTail != nil {
			t.Fatalf("discarded tail %v = %+v, %v", argv, got, err)
		}
	}
}

func TestProxyCommandPreservesOrdinaryEndOfFlags(t *testing.T) {
	for _, test := range []struct {
		argv []string
		row  string
	}{
		{[]string{"--", "--help"}, "ordinary_usage_error"},
		{[]string{"check", "--", "codex"}, "check"},
		{[]string{"check", "--", "--json"}, "ordinary_usage_error"},
	} {
		got, err := ClassifyProxyCommand(test.argv)
		if err != nil || got.Row != test.row {
			t.Fatalf("ClassifyProxyCommand(%v) = %+v, %v; want %s", test.argv, got, err, test.row)
		}
	}
}

func TestProxyCommandOperatorArgumentsAreTyped(t *testing.T) {
	id := strings.Repeat("b", 32)
	status, err := ClassifyProxyCommand([]string{"operation", "status", "--json", "--operation-id", id})
	if err != nil {
		t.Fatal(err)
	}
	statusArgs, ok := status.Arguments.(OperatorStatusArgumentsV1)
	if !ok || statusArgs.OperationID != id || !statusArgs.JSON {
		t.Fatalf("status arguments = %#v", status.Arguments)
	}
	recoverAuthority, err := ClassifyProxyCommand([]string{"operation", "recover", "--operation-id", id, "--json"})
	if err != nil {
		t.Fatal(err)
	}
	recoverArgs, ok := recoverAuthority.Arguments.(OperatorRecoverArgumentsV1)
	if !ok || recoverArgs.OperationID != id || !recoverArgs.JSON {
		t.Fatalf("recover arguments = %#v", recoverAuthority.Arguments)
	}
	for _, argv := range [][]string{
		{"operation", "recover", "--json"},
		{"operation", "status", "--operation-id", "BAD"},
		{"operation", "status", "--unknown", "secret"},
	} {
		got, classifyErr := ClassifyProxyCommand(argv)
		if classifyErr != nil || got.Row != "ordinary_usage_error" || !got.Terminating {
			t.Fatalf("invalid operation %v = %+v, %v", argv, got, classifyErr)
		}
	}
}

func TestProxyCommandClassifiesModelsAndCodexAuxiliaryCatalogues(t *testing.T) {
	tests := []struct {
		argv      []string
		catalogue string
		row       string
		deadline  CommandDeadlineV1
	}{
		{[]string{"models", "list", "--provider", "codex", "--json"}, "models", "models_list", CommandDeadlineV1{Total: 10 * time.Second, Forward: 10 * time.Second}},
		{[]string{"models", "refresh", "ignored"}, "models", "models_refresh", CommandDeadlineV1{Total: 60 * time.Second, Forward: 45 * time.Second, Reserve: 15 * time.Second}},
		{[]string{"models", "overlay", "add", "--provider", "codex", "--id", "gpt", "--clone-from", "base"}, "models", "models_overlay_add", CommandDeadlineV1{Total: 60 * time.Second, Forward: 45 * time.Second, Reserve: 15 * time.Second}},
		{[]string{"models", "overlay", "remove", "--provider", "anthropic", "--id", "claude"}, "models", "models_overlay_remove", CommandDeadlineV1{Total: 60 * time.Second, Forward: 45 * time.Second, Reserve: 15 * time.Second}},
		{[]string{"models", "overlay", "prune", "ignored"}, "models", "models_overlay_prune", CommandDeadlineV1{Total: 60 * time.Second, Forward: 45 * time.Second, Reserve: 15 * time.Second}},
		{[]string{"codex", "validate", "capture", "--input", "in", "--output", "out"}, "codex_auxiliary", "validate_capture", CommandDeadlineV1{Total: 30 * time.Second, Forward: 20 * time.Second, Reserve: 10 * time.Second}},
		{[]string{"codex", "validate", "http", "--client-build", "build"}, "codex_auxiliary", "validate_http", CommandDeadlineV1{Total: 10 * time.Second, Forward: 10 * time.Second}},
		{[]string{"codex", "validate", "websocket", "--client-build", "build", "--client-executable", "/bin/codex"}, "codex_auxiliary", "validate_websocket", CommandDeadlineV1{Total: 45 * time.Second, Forward: 30 * time.Second, Reserve: 15 * time.Second}},
		{[]string{"codex", "canary", "start"}, "codex_auxiliary", "canary_start", CommandDeadlineV1{Total: 30 * time.Second, Forward: 15 * time.Second, Reserve: 15 * time.Second}},
		{[]string{"codex", "canary", "status"}, "codex_auxiliary", "canary_status", CommandDeadlineV1{Total: 10 * time.Second, Forward: 10 * time.Second}},
		{[]string{"codex", "canary", "stop"}, "codex_auxiliary", "canary_stop", CommandDeadlineV1{Total: 30 * time.Second, Forward: 15 * time.Second, Reserve: 15 * time.Second}},
	}
	for _, test := range tests {
		got, err := ClassifyProxyCommand(test.argv)
		if err != nil || got.Catalogue != test.catalogue || got.Row != test.row || got.Deadline != test.deadline {
			t.Fatalf("ClassifyProxyCommand(%v) = %+v, %v", test.argv, got, err)
		}
	}
}

func TestProxyCommandUsesExactRefreshReadDeadlines(t *testing.T) {
	agent, err := ClassifyProxyCommand([]string{"agent", "install"})
	if err != nil {
		t.Fatal(err)
	}
	if agent.Deadline != (CommandDeadlineV1{Total: 30 * time.Second, Forward: 15 * time.Second, Reserve: 15 * time.Second}) {
		t.Fatalf("agent deadline = %+v", agent.Deadline)
	}
	id := strings.Repeat("c", 32)
	receipt, err := ClassifyProxyCommand([]string{"proxy", "candidate", "receipt", "show", "--instance-state-root", "/tmp/candidate", "--attempt-id", id})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Deadline != (CommandDeadlineV1{Total: 10 * time.Second, Forward: 10 * time.Second}) {
		t.Fatalf("candidate receipt deadline = %+v", receipt.Deadline)
	}
}

func TestProxyCommandBuildsTypedArgumentsAndDeadlines(t *testing.T) {
	got, err := ClassifyProxyCommand([]string{"--json", "--refresh", "check", "codex", "claude"})
	if err != nil {
		t.Fatal(err)
	}
	args, ok := got.Arguments.(OrdinaryCheckArgumentsV1)
	if !ok || !args.JSON || !args.Refresh || strings.Join(args.Providers, ",") != "codex,claude" {
		t.Fatalf("check args = %#v", got.Arguments)
	}
	if got.Deadline != (CommandDeadlineV1{Total: 120 * time.Second, Forward: 90 * time.Second, Reserve: 30 * time.Second}) {
		t.Fatalf("check deadline = %+v", got.Deadline)
	}

	login, err := ClassifyProxyCommand([]string{"--json", "--refresh", "claude", "login", "--activate"})
	if err != nil {
		t.Fatal(err)
	}
	loginArgs, ok := login.Arguments.(OrdinaryLoginArgumentsV1)
	if !ok || !loginArgs.Activate {
		t.Fatalf("login args retained global flags: %#v", login.Arguments)
	}
	if login.Deadline.Total != 360*time.Second || login.Deadline.Forward != 315*time.Second || login.Deadline.Reserve != 45*time.Second {
		t.Fatalf("login deadline = %+v", login.Deadline)
	}
}

func TestProxyCommandParsesCacheTTL(t *testing.T) {
	tests := map[string]int{"": 30, "+30": 30, "0030": 30, "-1": 0, "3601": 3600, " 30": 30, "0x10": 30, "1.5": 30, "bad": 30, "9223372036854775808": 30}
	for input, want := range tests {
		if got := ParseCheckCachePolicyV1(input, false); got.EffectiveTTLSeconds != want {
			t.Fatalf("TTL %q = %d, want %d", input, got.EffectiveTTLSeconds, want)
		}
	}
	if got := ParseCheckCachePolicyV1("30", true); got.InitialLookup != "bypass" {
		t.Fatalf("refresh lookup = %q, want bypass", got.InitialLookup)
	}
}

func TestProxyCommandClassifiesCandidateReceiptLookupBeforeState(t *testing.T) {
	id := strings.Repeat("a", 32)
	got, err := ClassifyProxyCommand([]string{"proxy", "candidate", "receipt", "show", "--instance-state-root", "/tmp/candidate", "--attempt-id", id, "--json"})
	if err != nil {
		t.Fatal(err)
	}
	args, ok := got.Arguments.(CandidateReceiptLookupArgumentsV1)
	if got.Row != "candidate_receipt_show" || !ok || args.InstanceStateRoot != "/tmp/candidate" || args.AttemptID != id || !args.JSON {
		t.Fatalf("candidate receipt authority = %+v", got)
	}
	invalid, err := ClassifyProxyCommand([]string{"proxy", "candidate", "receipt", "show", "--instance-state-root", "/tmp/candidate", "--attempt-id", "abc"})
	if err != nil || invalid.Row != "ordinary_usage_error" || !invalid.Terminating {
		t.Fatalf("malformed attempt ID = %+v, %v", invalid, err)
	}
	duplicate, err := ClassifyProxyCommand([]string{"proxy", "candidate", "receipt", "show", "--instance-state-root", "/tmp/one", "--instance-state-root", "/tmp/two", "--attempt-id", id})
	if err != nil || duplicate.Row != "ordinary_usage_error" || !duplicate.Terminating {
		t.Fatalf("duplicate selector = %+v, %v", duplicate, err)
	}
}

func TestProxyCommandClassifiesReconciledStatusAndRescue(t *testing.T) {
	status, err := ClassifyProxyCommand([]string{"proxy", "status", "--instance-state-root", "/tmp/instance", "--human", "--strict", "--timeout", "3s"})
	arguments, ok := status.Arguments.(ProxyStatusArgumentsV1)
	if err != nil || status.Row != "proxy_status" || !ok || arguments.InstanceStateRoot != "/tmp/instance" || !arguments.Human || !arguments.Strict || arguments.Timeout != 3*time.Second || status.Deadline.Total != 3*time.Second {
		t.Fatalf("status authority = %+v args=%+v err=%v", status, arguments, err)
	}
	rescue, err := ClassifyProxyCommand([]string{"proxy", "rescue", "status", "--port", "29280"})
	if err != nil || rescue.Row != "proxy_rescue" || rescue.Terminating {
		t.Fatalf("rescue authority = %+v err=%v", rescue, err)
	}
}
