package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/jacobcxdev/cq/internal/cli"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
)

type terminalRoutingFixture struct {
	terminalFixtureRegistration
	Exit                  int
	Code, Message, Result string
}

var terminalRoutingFixtures = []terminalRoutingFixture{
	{terminalFixtureRegistration{"cq claude proxy pin clear", "Success", "stdout", "text/cq-claude-proxy-pin-clear/02-success.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq claude proxy pin clear", "No pin configured", "stdout", "text/cq-claude-proxy-pin-clear/03-no-pin-configured.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq claude proxy pin clear", "Failure", "stderr", "text/cq-claude-proxy-pin-clear/04-failure.txt"}, 1, "routing_io_failed", "Routing operation failed: cannot read routing configuration.", ""},
	{terminalFixtureRegistration{"cq claude proxy pin clear", "Timeout", "stderr", "text/cq-claude-proxy-pin-clear/05-timeout.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", ""},
	{terminalFixtureRegistration{"cq claude proxy pin clear", "Timeout", "stdout", "text/cq-claude-proxy-pin-clear/06-timeout.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", ""},
	{terminalFixtureRegistration{"cq claude proxy pin set", "Success", "stdout", "text/cq-claude-proxy-pin-set/02-success.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq claude proxy pin set", "Failure", "stderr", "text/cq-claude-proxy-pin-set/03-failure.txt"}, 3, "routing_account_not_found", "Account not found: sam@example.test.", ""},
	{terminalFixtureRegistration{"cq claude proxy pin set", "Timeout", "stderr", "text/cq-claude-proxy-pin-set/04-timeout.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", ""},
	{terminalFixtureRegistration{"cq claude proxy pin set", "Timeout", "stdout", "text/cq-claude-proxy-pin-set/05-timeout.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", ""},
	{terminalFixtureRegistration{"cq claude proxy pin show", "Success", "stdout", "text/cq-claude-proxy-pin-show/02-success.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq claude proxy pin show", "No pin configured", "stdout", "text/cq-claude-proxy-pin-show/03-no-pin-configured.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq claude proxy pin show", "Failure", "stderr", "text/cq-claude-proxy-pin-show/04-failure.txt"}, 1, "routing_io_failed", "Routing operation failed: cannot read routing configuration.", ""},
	{terminalFixtureRegistration{"cq codex proxy fallback clear", "Success", "stdout", "text/cq-codex-proxy-fallback-clear/02-success.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy fallback clear", "No-op · already absent", "stdout", "text/cq-codex-proxy-fallback-clear/03-no-op-already-absent.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy fallback clear", "Failure", "stderr", "text/cq-codex-proxy-fallback-clear/04-failure.txt"}, 1, "routing_io_failed", "Routing operation failed: load config.", ""},
	{terminalFixtureRegistration{"cq codex proxy fallback clear", "Timeout · no receipt", "stderr", "text/cq-codex-proxy-fallback-clear/05-timeout-no-receipt.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", "unconfirmed"},
	{terminalFixtureRegistration{"cq codex proxy fallback set", "Success", "stdout", "text/cq-codex-proxy-fallback-set/02-success.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy fallback set", "No-op · same resolved account", "stdout", "text/cq-codex-proxy-fallback-set/03-no-op-same-resolved-account.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy fallback set", "Failure", "stderr", "text/cq-codex-proxy-fallback-set/04-failure.txt"}, 6, "routing_account_ambiguous", "Account reference is ambiguous; use a unique alias or AccountKey.", ""},
	{terminalFixtureRegistration{"cq codex proxy fallback set", "Timeout · no receipt", "stderr", "text/cq-codex-proxy-fallback-set/05-timeout-no-receipt.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", "unconfirmed"},
	{terminalFixtureRegistration{"cq codex proxy fallback show", "Success", "stdout", "text/cq-codex-proxy-fallback-show/02-success.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy fallback show", "Empty · no configured fallback", "stdout", "text/cq-codex-proxy-fallback-show/03-empty-no-configured-fallback.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy fallback show", "Failure", "stderr", "text/cq-codex-proxy-fallback-show/04-failure.txt"}, 1, "routing_io_failed", "Routing operation failed: load config.", ""},
	{terminalFixtureRegistration{"cq codex proxy fallback show", "Timeout · observation unavailable", "stderr", "text/cq-codex-proxy-fallback-show/05-timeout-observation-unavailable.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", "unavailable"},
	{terminalFixtureRegistration{"cq codex proxy pin clear", "Success", "stdout", "text/cq-codex-proxy-pin-clear/02-success.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy pin clear", "No-op · already absent", "stdout", "text/cq-codex-proxy-pin-clear/03-no-op-already-absent.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy pin clear", "Failure", "stderr", "text/cq-codex-proxy-pin-clear/04-failure.txt"}, 1, "routing_io_failed", "Routing operation failed: load config.", ""},
	{terminalFixtureRegistration{"cq codex proxy pin clear", "Timeout · no receipt", "stderr", "text/cq-codex-proxy-pin-clear/05-timeout-no-receipt.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", "unconfirmed"},
	{terminalFixtureRegistration{"cq codex proxy pin set", "Success", "stdout", "text/cq-codex-proxy-pin-set/02-success.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy pin set", "No-op · same resolved account", "stdout", "text/cq-codex-proxy-pin-set/03-no-op-same-resolved-account.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy pin set", "Failure", "stderr", "text/cq-codex-proxy-pin-set/04-failure.txt"}, 6, "routing_account_ambiguous", "Account reference is ambiguous; use a unique alias or AccountKey.", ""},
	{terminalFixtureRegistration{"cq codex proxy pin set", "Timeout · no receipt", "stderr", "text/cq-codex-proxy-pin-set/05-timeout-no-receipt.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", "unconfirmed"},
	{terminalFixtureRegistration{"cq codex proxy pin show", "Success", "stdout", "text/cq-codex-proxy-pin-show/02-success.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy pin show", "Empty · no configured pin", "stdout", "text/cq-codex-proxy-pin-show/03-empty-no-configured-pin.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy pin show", "Failure", "stderr", "text/cq-codex-proxy-pin-show/04-failure.txt"}, 1, "routing_io_failed", "Routing operation failed: load config.", ""},
	{terminalFixtureRegistration{"cq codex proxy pin show", "Timeout · observation unavailable", "stderr", "text/cq-codex-proxy-pin-show/05-timeout-observation-unavailable.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", "unavailable"},
	{terminalFixtureRegistration{"cq codex proxy lease redistribute", "Success", "stdout", "text/cq-codex-proxy-lease-redistribute/02-success.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy lease redistribute", "Empty · no reusable affinity", "stdout", "text/cq-codex-proxy-lease-redistribute/03-empty-no-reusable-affinity.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy lease redistribute", "Failure", "stderr", "text/cq-codex-proxy-lease-redistribute/04-failure.txt"}, 4, "lease_journal_unavailable", "Lease journal is unavailable for redistribution.", ""},
	{terminalFixtureRegistration{"cq codex proxy lease redistribute", "Timeout · no receipt", "stderr", "text/cq-codex-proxy-lease-redistribute/05-timeout-no-receipt.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", "unconfirmed"},
	{terminalFixtureRegistration{"cq codex proxy lease invalidate", "Success", "stdout", "text/cq-codex-proxy-lease-invalidate/02-success.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy lease invalidate", "No-op · no reusable affinity", "stdout", "text/cq-codex-proxy-lease-invalidate/03-no-op-no-reusable-affinity.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy lease invalidate", "Failure", "stderr", "text/cq-codex-proxy-lease-invalidate/04-failure.txt"}, 4, "lease_journal_unavailable", "Lease journal is unavailable for invalidation.", ""},
	{terminalFixtureRegistration{"cq codex proxy lease invalidate", "Timeout · no receipt", "stderr", "text/cq-codex-proxy-lease-invalidate/05-timeout-no-receipt.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", "unconfirmed"},
	{terminalFixtureRegistration{"cq codex proxy policy apply", "Success", "stdout", "text/cq-codex-proxy-policy-apply/02-success.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy policy apply", "Failure", "stderr", "text/cq-codex-proxy-policy-apply/03-failure.txt"}, 6, "policy_generation_conflict", "Policy generations conflict; show current policy and prepare a fresh document.", ""},
	{terminalFixtureRegistration{"cq codex proxy policy apply", "Timeout · no receipt", "stderr", "text/cq-codex-proxy-policy-apply/04-timeout-no-receipt.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", "unconfirmed"},
	{terminalFixtureRegistration{"cq codex proxy policy show", "Success", "stdout", "text/cq-codex-proxy-policy-show/02-success.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy policy show", "Empty · published policy without collections", "stdout", "text/cq-codex-proxy-policy-show/03-empty-published-policy-without-collections.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy policy show", "Offline · application not checked", "stdout", "text/cq-codex-proxy-policy-show/04-offline-application-not-checked.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy policy show", "Success · retained capability and delegation evidence", "stdout", "text/cq-codex-proxy-policy-show/05-success-retained-capability-and-delegation-evidence.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy policy show", "Failure", "stderr", "text/cq-codex-proxy-policy-show/06-failure.txt"}, 4, "routing_control_unavailable", "Running CQ proxy control is unavailable.", ""},
	{terminalFixtureRegistration{"cq codex proxy policy show", "Timeout · observation unavailable", "stderr", "text/cq-codex-proxy-policy-show/07-timeout-observation-unavailable.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", "unavailable"},
	{terminalFixtureRegistration{"cq codex proxy pool rename", "Success", "stdout", "text/cq-codex-proxy-pool-rename/02-success.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy pool rename", "Failure", "stderr", "text/cq-codex-proxy-pool-rename/03-failure.txt"}, 6, "pool_name_conflict", "Pool name already exists: primary.", ""},
	{terminalFixtureRegistration{"cq codex proxy pool rename", "Timeout · no receipt", "stderr", "text/cq-codex-proxy-pool-rename/04-timeout-no-receipt.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", "unconfirmed"},
	{terminalFixtureRegistration{"cq codex proxy pool set", "Success", "stdout", "text/cq-codex-proxy-pool-set/02-success.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy pool set", "No-op · equivalent policy", "stdout", "text/cq-codex-proxy-pool-set/03-no-op-equivalent-policy.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy pool set", "Failure", "stderr", "text/cq-codex-proxy-pool-set/04-failure.txt"}, 6, "routing_account_ambiguous", "Account reference is ambiguous; use a unique alias or AccountKey.", ""},
	{terminalFixtureRegistration{"cq codex proxy pool set", "Timeout · no receipt", "stderr", "text/cq-codex-proxy-pool-set/05-timeout-no-receipt.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", "unconfirmed"},
	{terminalFixtureRegistration{"cq codex proxy pool value", "Success", "stdout", "text/cq-codex-proxy-pool-value/02-success.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy pool value", "No-op · equivalent policy", "stdout", "text/cq-codex-proxy-pool-value/03-no-op-equivalent-policy.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy pool value", "Failure", "stderr", "text/cq-codex-proxy-pool-value/04-failure.txt"}, 3, "pool_not_found", "Pool not found: work.", ""},
	{terminalFixtureRegistration{"cq codex proxy pool value", "Timeout · no receipt", "stderr", "text/cq-codex-proxy-pool-value/05-timeout-no-receipt.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", "unconfirmed"},
	{terminalFixtureRegistration{"cq codex proxy prime disable", "Success", "stdout", "text/cq-codex-proxy-prime-disable/02-success.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy prime disable", "No-op · same configured state", "stdout", "text/cq-codex-proxy-prime-disable/03-no-op-same-configured-state.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy prime disable", "Failure", "stderr", "text/cq-codex-proxy-prime-disable/04-failure.txt"}, 1, "routing_io_failed", "Routing operation failed: load config.", ""},
	{terminalFixtureRegistration{"cq codex proxy prime disable", "Timeout · no receipt", "stderr", "text/cq-codex-proxy-prime-disable/05-timeout-no-receipt.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", "unconfirmed"},
	{terminalFixtureRegistration{"cq codex proxy prime enable", "Success", "stdout", "text/cq-codex-proxy-prime-enable/02-success.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy prime enable", "No-op · same configured state", "stdout", "text/cq-codex-proxy-prime-enable/03-no-op-same-configured-state.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy prime enable", "Failure", "stderr", "text/cq-codex-proxy-prime-enable/04-failure.txt"}, 1, "routing_io_failed", "Routing operation failed: load config.", ""},
	{terminalFixtureRegistration{"cq codex proxy prime enable", "Timeout · no receipt", "stderr", "text/cq-codex-proxy-prime-enable/05-timeout-no-receipt.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", "unconfirmed"},
	{terminalFixtureRegistration{"cq codex proxy prime status", "Success", "stdout", "text/cq-codex-proxy-prime-status/02-success.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy prime status", "Empty · no model overrides", "stdout", "text/cq-codex-proxy-prime-status/03-empty-no-model-overrides.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy prime status", "Failure", "stderr", "text/cq-codex-proxy-prime-status/04-failure.txt"}, 1, "routing_io_failed", "Routing operation failed: load config.", ""},
	{terminalFixtureRegistration{"cq codex proxy prime status", "Timeout · observation unavailable", "stderr", "text/cq-codex-proxy-prime-status/05-timeout-observation-unavailable.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", "unavailable"},
	{terminalFixtureRegistration{"cq codex proxy reserve clear", "Success", "stdout", "text/cq-codex-proxy-reserve-clear/02-success.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy reserve clear", "No-op · already unconfigured", "stdout", "text/cq-codex-proxy-reserve-clear/03-no-op-already-unconfigured.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy reserve clear", "Failure", "stderr", "text/cq-codex-proxy-reserve-clear/04-failure.txt"}, 4, "routing_control_unavailable", "Running CQ proxy control is unavailable.", ""},
	{terminalFixtureRegistration{"cq codex proxy reserve clear", "Timeout · no receipt", "stderr", "text/cq-codex-proxy-reserve-clear/05-timeout-no-receipt.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", "unconfirmed"},
	{terminalFixtureRegistration{"cq codex proxy reserve disable", "Success", "stdout", "text/cq-codex-proxy-reserve-disable/02-success.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy reserve disable", "Failure", "stderr", "text/cq-codex-proxy-reserve-disable/03-failure.txt"}, 6, "reserve_evidence_required", "Fresh usage and reset evidence are required to disable the reserve.", ""},
	{terminalFixtureRegistration{"cq codex proxy reserve disable", "Timeout · no receipt", "stderr", "text/cq-codex-proxy-reserve-disable/04-timeout-no-receipt.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", "unconfirmed"},
	{terminalFixtureRegistration{"cq codex proxy reserve enable", "Success", "stdout", "text/cq-codex-proxy-reserve-enable/02-success.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy reserve enable", "No-op · already enabled", "stdout", "text/cq-codex-proxy-reserve-enable/03-no-op-already-enabled.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy reserve enable", "Failure", "stderr", "text/cq-codex-proxy-reserve-enable/04-failure.txt"}, 6, "reserve_not_configured", "Reserve is not configured; use reserve set first.", ""},
	{terminalFixtureRegistration{"cq codex proxy reserve enable", "Timeout · no receipt", "stderr", "text/cq-codex-proxy-reserve-enable/05-timeout-no-receipt.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", "unconfirmed"},
	{terminalFixtureRegistration{"cq codex proxy reserve set", "Success", "stdout", "text/cq-codex-proxy-reserve-set/02-success.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy reserve set", "Failure", "stderr", "text/cq-codex-proxy-reserve-set/03-failure.txt"}, 6, "reserve_window_unavailable", "Selected window is unavailable; inspect reserve windows.", ""},
	{terminalFixtureRegistration{"cq codex proxy reserve set", "Timeout · no receipt", "stderr", "text/cq-codex-proxy-reserve-set/04-timeout-no-receipt.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", "unconfirmed"},
	{terminalFixtureRegistration{"cq codex proxy reserve status", "Success", "stdout", "text/cq-codex-proxy-reserve-status/02-success.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy reserve status", "Threshold reached · valid observation", "stdout", "text/cq-codex-proxy-reserve-status/03-threshold-reached-valid-observation.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy reserve status", "Unavailable observation · fail-closed reserve", "stdout", "text/cq-codex-proxy-reserve-status/04-unavailable-observation-fail-closed-reserve.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy reserve status", "Empty · unconfigured reserve", "stdout", "text/cq-codex-proxy-reserve-status/05-empty-unconfigured-reserve.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy reserve status", "Failure", "stderr", "text/cq-codex-proxy-reserve-status/06-failure.txt"}, 4, "routing_control_unavailable", "Running CQ proxy control is unavailable.", ""},
	{terminalFixtureRegistration{"cq codex proxy reserve status", "Timeout · observation unavailable", "stderr", "text/cq-codex-proxy-reserve-status/07-timeout-observation-unavailable.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", "unavailable"},
	{terminalFixtureRegistration{"cq codex proxy reserve windows", "Success", "stdout", "text/cq-codex-proxy-reserve-windows/02-success.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy reserve windows", "Empty · no observed windows", "stdout", "text/cq-codex-proxy-reserve-windows/03-empty-no-observed-windows.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy reserve windows", "Failure", "stderr", "text/cq-codex-proxy-reserve-windows/04-failure.txt"}, 4, "routing_control_unavailable", "Running CQ proxy control is unavailable.", ""},
	{terminalFixtureRegistration{"cq codex proxy reserve windows", "Timeout · observation unavailable", "stderr", "text/cq-codex-proxy-reserve-windows/05-timeout-observation-unavailable.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", "unavailable"},
	{terminalFixtureRegistration{"cq codex proxy session bind", "Success", "stdout", "text/cq-codex-proxy-session-bind/02-success.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy session bind", "No-op · same pool binding", "stdout", "text/cq-codex-proxy-session-bind/03-no-op-same-pool-binding.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy session bind", "Failure", "stderr", "text/cq-codex-proxy-session-bind/04-failure.txt"}, 3, "pool_not_found", "Pool not found: work.", ""},
	{terminalFixtureRegistration{"cq codex proxy session bind", "Timeout · no receipt", "stderr", "text/cq-codex-proxy-session-bind/05-timeout-no-receipt.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", "unconfirmed"},
	{terminalFixtureRegistration{"cq codex proxy session digest", "Success", "stdout", "text/cq-codex-proxy-session-digest/02-success.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy session digest", "Failure", "stderr", "text/cq-codex-proxy-session-digest/03-failure.txt"}, 5, "routing_auth_failed", "Local proxy authentication failed.", ""},
	{terminalFixtureRegistration{"cq codex proxy session digest", "Timeout · observation unavailable", "stderr", "text/cq-codex-proxy-session-digest/04-timeout-observation-unavailable.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", "unavailable"},
	{terminalFixtureRegistration{"cq codex proxy session list", "Success", "stdout", "text/cq-codex-proxy-session-list/02-success.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy session list", "Empty · no bindings", "stdout", "text/cq-codex-proxy-session-list/03-empty-no-bindings.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy session list", "Failure", "stderr", "text/cq-codex-proxy-session-list/04-failure.txt"}, 4, "routing_control_unavailable", "Running CQ proxy control is unavailable.", ""},
	{terminalFixtureRegistration{"cq codex proxy session list", "Timeout · observation unavailable", "stderr", "text/cq-codex-proxy-session-list/05-timeout-observation-unavailable.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", "unavailable"},
	{terminalFixtureRegistration{"cq codex proxy session show", "Success", "stdout", "text/cq-codex-proxy-session-show/02-success.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy session show", "Failure", "stderr", "text/cq-codex-proxy-session-show/03-failure.txt"}, 3, "session_binding_not_found", "Session binding not found.", ""},
	{terminalFixtureRegistration{"cq codex proxy session show", "Timeout · observation unavailable", "stderr", "text/cq-codex-proxy-session-show/04-timeout-observation-unavailable.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", "unavailable"},
	{terminalFixtureRegistration{"cq codex proxy session unbind", "Success", "stdout", "text/cq-codex-proxy-session-unbind/02-success.txt"}, 0, "", "", ""},
	{terminalFixtureRegistration{"cq codex proxy session unbind", "Failure", "stderr", "text/cq-codex-proxy-session-unbind/03-failure.txt"}, 3, "session_binding_not_found", "Session binding not found.", ""},
	{terminalFixtureRegistration{"cq codex proxy session unbind", "Timeout · no receipt", "stderr", "text/cq-codex-proxy-session-unbind/04-timeout-no-receipt.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", "unconfirmed"},
	{terminalFixtureRegistration{"cq codex proxy fallback clear", "Timeout · saved configuration", "stdout", "text/cq-codex-proxy-fallback-clear/06-timeout-saved-configuration.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", ""},
	{terminalFixtureRegistration{"cq codex proxy fallback clear", "Timeout · saved configuration", "stderr", "text/cq-codex-proxy-fallback-clear/07-timeout-saved-configuration.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", ""},
	{terminalFixtureRegistration{"cq codex proxy fallback clear", "Interruption · saved configuration", "stdout", "text/cq-codex-proxy-fallback-clear/08-interruption-saved-configuration.txt"}, 130, "interrupted", "Operation interrupted; inspect state before retrying.", "completion unknown"},
	{terminalFixtureRegistration{"cq codex proxy fallback clear", "Interruption · saved configuration", "stderr", "text/cq-codex-proxy-fallback-clear/09-interruption-saved-configuration.txt"}, 130, "interrupted", "Operation interrupted; inspect state before retrying.", "completion unknown"},
	{terminalFixtureRegistration{"cq codex proxy fallback set", "Timeout · saved configuration", "stdout", "text/cq-codex-proxy-fallback-set/06-timeout-saved-configuration.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", ""},
	{terminalFixtureRegistration{"cq codex proxy fallback set", "Timeout · saved configuration", "stderr", "text/cq-codex-proxy-fallback-set/07-timeout-saved-configuration.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", ""},
	{terminalFixtureRegistration{"cq codex proxy fallback set", "Interruption · saved configuration", "stdout", "text/cq-codex-proxy-fallback-set/08-interruption-saved-configuration.txt"}, 130, "interrupted", "Operation interrupted; inspect state before retrying.", "completion unknown"},
	{terminalFixtureRegistration{"cq codex proxy fallback set", "Interruption · saved configuration", "stderr", "text/cq-codex-proxy-fallback-set/09-interruption-saved-configuration.txt"}, 130, "interrupted", "Operation interrupted; inspect state before retrying.", "completion unknown"},
	{terminalFixtureRegistration{"cq codex proxy pin clear", "Timeout · saved configuration", "stdout", "text/cq-codex-proxy-pin-clear/06-timeout-saved-configuration.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", ""},
	{terminalFixtureRegistration{"cq codex proxy pin clear", "Timeout · saved configuration", "stderr", "text/cq-codex-proxy-pin-clear/07-timeout-saved-configuration.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", ""},
	{terminalFixtureRegistration{"cq codex proxy pin clear", "Interruption · saved configuration", "stdout", "text/cq-codex-proxy-pin-clear/08-interruption-saved-configuration.txt"}, 130, "interrupted", "Operation interrupted; inspect state before retrying.", "completion unknown"},
	{terminalFixtureRegistration{"cq codex proxy pin clear", "Interruption · saved configuration", "stderr", "text/cq-codex-proxy-pin-clear/09-interruption-saved-configuration.txt"}, 130, "interrupted", "Operation interrupted; inspect state before retrying.", "completion unknown"},
	{terminalFixtureRegistration{"cq codex proxy pin set", "Timeout · saved configuration", "stdout", "text/cq-codex-proxy-pin-set/06-timeout-saved-configuration.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", ""},
	{terminalFixtureRegistration{"cq codex proxy pin set", "Timeout · saved configuration", "stderr", "text/cq-codex-proxy-pin-set/07-timeout-saved-configuration.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", ""},
	{terminalFixtureRegistration{"cq codex proxy pin set", "Interruption · saved configuration", "stdout", "text/cq-codex-proxy-pin-set/08-interruption-saved-configuration.txt"}, 130, "interrupted", "Operation interrupted; inspect state before retrying.", "completion unknown"},
	{terminalFixtureRegistration{"cq codex proxy pin set", "Interruption · saved configuration", "stderr", "text/cq-codex-proxy-pin-set/09-interruption-saved-configuration.txt"}, 130, "interrupted", "Operation interrupted; inspect state before retrying.", "completion unknown"},
	{terminalFixtureRegistration{"cq codex proxy prime disable", "Timeout · saved configuration", "stdout", "text/cq-codex-proxy-prime-disable/06-timeout-saved-configuration.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", ""},
	{terminalFixtureRegistration{"cq codex proxy prime disable", "Timeout · saved configuration", "stderr", "text/cq-codex-proxy-prime-disable/07-timeout-saved-configuration.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", ""},
	{terminalFixtureRegistration{"cq codex proxy prime disable", "Interruption · saved configuration", "stdout", "text/cq-codex-proxy-prime-disable/08-interruption-saved-configuration.txt"}, 130, "interrupted", "Operation interrupted; inspect state before retrying.", "completion unknown"},
	{terminalFixtureRegistration{"cq codex proxy prime disable", "Interruption · saved configuration", "stderr", "text/cq-codex-proxy-prime-disable/09-interruption-saved-configuration.txt"}, 130, "interrupted", "Operation interrupted; inspect state before retrying.", "completion unknown"},
	{terminalFixtureRegistration{"cq codex proxy prime enable", "Timeout · saved configuration", "stdout", "text/cq-codex-proxy-prime-enable/06-timeout-saved-configuration.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", ""},
	{terminalFixtureRegistration{"cq codex proxy prime enable", "Timeout · saved configuration", "stderr", "text/cq-codex-proxy-prime-enable/07-timeout-saved-configuration.txt"}, 7, "routing_timeout", "Routing operation timed out; inspect current state before retrying a mutation.", ""},
	{terminalFixtureRegistration{"cq codex proxy prime enable", "Interruption · saved configuration", "stdout", "text/cq-codex-proxy-prime-enable/08-interruption-saved-configuration.txt"}, 130, "interrupted", "Operation interrupted; inspect state before retrying.", "completion unknown"},
	{terminalFixtureRegistration{"cq codex proxy prime enable", "Interruption · saved configuration", "stderr", "text/cq-codex-proxy-prime-enable/09-interruption-saved-configuration.txt"}, 130, "interrupted", "Operation interrupted; inspect state before retrying.", "completion unknown"},
}

type routingIndex struct {
	Fixtures []struct {
		Command, Scenario, Stream, Invocation, File string
		Exit                                        json.RawMessage
	} `json:"fixtures"`
}

func TestTerminalRoutingManifestParity(t *testing.T) {
	manifestBytes, err := os.ReadFile(filepath.Join("..", "..", "docs", "superpowers", "plans", "terminal-presentation-coverage.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Fixtures []struct {
			Command, Scenario, Stream, File, Task string
		} `json:"fixtures"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	indexBytes, err := os.ReadFile(filepath.Join("..", "..", "specs", "cli-v2", "presentation", "examples", "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index routingIndex
	if err := json.Unmarshal(indexBytes, &index); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Fixtures) != len(index.Fixtures) {
		t.Errorf("manifest has %d fixtures; catalogue has %d", len(manifest.Fixtures), len(index.Fixtures))
	}
	indexed := make(map[string]terminalFixtureRegistration, len(index.Fixtures))
	for _, entry := range index.Fixtures {
		if _, exists := indexed[entry.File]; exists {
			t.Errorf("duplicate catalogue fixture %s", entry.File)
		}
		indexed[entry.File] = terminalFixtureRegistration{entry.Command, entry.Scenario, entry.Stream, entry.File}
	}
	registered := make(map[string]terminalFixtureRegistration, len(terminalRoutingFixtures))
	for _, fixture := range terminalRoutingFixtures {
		registered[fixture.File] = fixture.terminalFixtureRegistration
	}
	seen := make(map[string]bool, len(manifest.Fixtures))
	routingCount := 0
	for _, entry := range manifest.Fixtures {
		if seen[entry.File] {
			t.Errorf("duplicate manifest fixture %s", entry.File)
		}
		seen[entry.File] = true
		want := terminalFixtureRegistration{entry.Command, entry.Scenario, entry.Stream, entry.File}
		if got, ok := indexed[entry.File]; !ok || got != want {
			t.Errorf("catalogue mismatch for %s", entry.File)
		}
		if entry.Task == "T07" {
			routingCount++
			if got, ok := registered[entry.File]; !ok || got != want {
				t.Errorf("routing registration mismatch for %s", entry.File)
			}
		}
	}
	for file := range indexed {
		if !seen[file] {
			t.Errorf("catalogue fixture missing from manifest: %s", file)
		}
	}
	if routingCount != len(registered) {
		t.Errorf("manifest owns %d routing fixtures; tests register %d", routingCount, len(registered))
	}
}

func TestTerminalRoutingFixtures(t *testing.T) {
	indexBytes, err := os.ReadFile(filepath.Join("..", "..", "specs", "cli-v2", "presentation", "examples", "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index routingIndex
	if err := json.Unmarshal(indexBytes, &index); err != nil {
		t.Fatal(err)
	}
	byFile := make(map[string]struct {
		command, scenario, stream, invocation string
		exit                                  int
	})
	for _, entry := range index.Fixtures {
		var exit int
		if err := json.Unmarshal(entry.Exit, &exit); err != nil {
			var value string
			if json.Unmarshal(entry.Exit, &value) == nil {
				exit, _ = strconv.Atoi(value)
			}
		}
		byFile[entry.File] = struct {
			command, scenario, stream, invocation string
			exit                                  int
		}{entry.Command, entry.Scenario, entry.Stream, entry.Invocation, exit}
	}
	seen := make(map[string]bool)
	commands := make(map[string]bool)
	companions := make(map[string]string)
	for _, fixture := range terminalRoutingFixtures {
		companions[fixture.Command+"/"+fixture.Scenario+"/"+fixture.Stream] = fixture.File
	}
	for _, fixture := range terminalRoutingFixtures {
		commands[fixture.Command] = true
		entry, ok := byFile[fixture.File]
		if !ok || entry.command != fixture.Command || entry.scenario != fixture.Scenario || entry.stream != fixture.Stream || entry.exit != fixture.Exit {
			t.Errorf("fixture index mismatch: %+v", fixture)
			continue
		}
		if seen[fixture.File] {
			t.Errorf("duplicate fixture %s", fixture.File)
		}
		seen[fixture.File] = true
		for _, width := range []int{20, 40, 80, 100, 120} {
			t.Run(fmt.Sprintf("%s/%d", fixture.File, width), func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				o := terminalFixtureOptions(width)
				session := &cli.Session{Out: &stdout, Err: &stderr, In: strings.NewReader(""), Interactive: true, OutTerminal: o, ErrTerminal: o}
				invocation := entry.invocation
				if !strings.HasPrefix(invocation, "cq ") {
					invocation = fixture.Command + " --digest " + strings.Repeat("0123456789abcdef", 4)
				}
				argv := strings.Fields(strings.TrimPrefix(invocation, "cq "))
				exit := cli.Run(context.Background(), argv, session, func(path string) (cli.Handler, bool) {
					return func(context.Context, cli.Invocation, *cli.Session) cli.Outcome { return routingFixtureOutcome(fixture) }, true
				})
				if exit != fixture.Exit {
					t.Fatalf("exit %d want %d; stderr=%q", exit, fixture.Exit, stderr.String())
				}
				got := stdout.String()
				other := stderr.String()
				if fixture.Stream == "stderr" {
					got, other = other, got
				}
				if other != "" {
					stream := "stderr"
					if fixture.Stream == "stderr" {
						stream = "stdout"
					}
					companion := companions[fixture.Command+"/"+fixture.Scenario+"/"+stream]
					if companion == "" {
						t.Fatalf("unexpected other stream: %q", other)
					}
					if width >= 100 {
						assertTerminalFixture(t, companion, other)
					}
					for _, line := range strings.Split(other, "\n") {
						if ansi.StringWidth(line) > min(width, 100) {
							t.Fatalf("other stream line exceeds %d cells: %q", width, line)
						}
					}
				}
				if width >= 100 {
					assertTerminalFixture(t, fixture.File, got)
				}
				for _, line := range strings.Split(got, "\n") {
					if ansi.StringWidth(line) > min(width, 100) {
						t.Fatalf("line exceeds %d cells: %q", width, line)
					}
				}
				if got != "" && !strings.Contains(fixture.Scenario, "no bindings") && !strings.Contains(got, "\x1b[") {
					t.Fatal("expected semantic colour")
				}
			})
		}
	}
	if len(commands) != 30 || len(seen) != 141 {
		t.Errorf("routing coverage: %d commands, %d fixtures; want 30 and 141", len(commands), len(seen))
	}
	for _, entry := range index.Fixtures {
		if commands[entry.Command] && entry.Scenario != "Help" && !seen[entry.File] {
			t.Errorf("unregistered routing scenario: %s", entry.File)
		}
	}
}

func routingFixtureOutcome(f terminalRoutingFixture) cli.Outcome {
	if f.Exit != 0 {
		out := cli.Outcome{ExitCode: f.Exit, Errors: []cli.Diagnostic{{Code: f.Code, Message: f.Message}}, TerminalResult: f.Result}
		if strings.HasSuffix(f.Scenario, "saved configuration") {
			parts := strings.Fields(f.Command)
			last := parts[len(parts)-1]
			var receipt cli.Outcome
			if strings.Contains(f.Command, " prime ") {
				receipt = cli.Outcome{Terminal: func(o cli.TerminalOptions) string {
					return terminalPrime(last == "enable", map[string]string{"gpt-reserve": "gpt-5.4"}, last, o)
				}}
			} else {
				data := v2SelectionData{Provider: "codex", Kind: parts[3], Configured: last == "set"}
				if data.Configured {
					account := "acct-alex"
					data.Account = &account
				}
				receipt = cli.Outcome{Terminal: func(o cli.TerminalOptions) string {
					return terminalSelection(data, last, o)
				}}
			}
			return v2CodexSelectionSavedStop(out, receipt)
		}
		if f.Code == "routing_timeout" && strings.HasPrefix(f.Command, "cq claude proxy pin ") {
			out.ConfirmedReceipt = true
			action := strings.TrimPrefix(f.Command, "cq claude proxy pin ")
			data := v2SelectionData{Provider: "claude", Kind: "pin", Configured: action == "set"}
			if data.Configured {
				account := "alex@example.test"
				data.Account = &account
			}
			out.Terminal = func(o cli.TerminalOptions) string {
				return terminalSelection(data, action, o) + routingLine(o, 2, 17, "Live application", routingAttention("unconfirmed"))
			}
		}
		return out
	}
	parts := strings.Fields(f.Command)
	last := parts[len(parts)-1]
	scenario := f.Scenario
	switch {
	case f.Command == "cq codex proxy lease redistribute":
		body := `{"scheduled_leases":1240,"deferred_leases":12,"journal_generation":42}`
		if strings.HasPrefix(scenario, "Empty") {
			body = `{"scheduled_leases":0,"deferred_leases":0,"journal_generation":43}`
		}
		return handleV2RoutingDiagnosticsWithDependencies(context.Background(), cli.Invocation{Path: "codex proxy lease redistribute", Options: map[string][]string{"timeout": {"10s"}}}, &cli.Session{}, diagnosticsLeaseResponse(body))
	case strings.Contains(f.Command, " lease "):
		count := 1240
		if strings.Contains(scenario, "No-op") {
			count = 0
		}
		generation := uint64(42)
		if count == 0 {
			generation = 43
		}
		r := proxy.CodexLeaseInvalidationResult{InvalidatedLeases: count, JournalGeneration: generation}
		return cli.Outcome{Terminal: func(o cli.TerminalOptions) string { return terminalLease(r, o) }}
	case strings.Contains(f.Command, " reserve "):
		data := routingFixtureReserve(f)
		return cli.Outcome{Terminal: func(o cli.TerminalOptions) string { return terminalReserve(data, last, o) }}
	case strings.Contains(f.Command, " prime "):
		enabled := last != "disable" && !strings.Contains(scenario, "no model overrides")
		overrides := map[string]string{"gpt-reserve": "gpt-5.4"}
		if strings.Contains(scenario, "no model overrides") {
			overrides = nil
		}
		return cli.Outcome{Terminal: func(o cli.TerminalOptions) string { return terminalPrime(enabled, overrides, last, o) }}
	case strings.Contains(f.Command, " pin ") || strings.Contains(f.Command, " fallback "):
		provider, kind := parts[1], parts[3]
		configured := last == "set" || last == "show" && scenario == "Success"
		var account *string
		if configured {
			value := "acct-alex"
			if provider == "claude" {
				value = "alex@example.test"
			}
			account = &value
		}
		data := v2SelectionData{Provider: provider, Kind: kind, Configured: configured, Account: account}
		return cli.Outcome{Terminal: func(o cli.TerminalOptions) string { return terminalSelection(data, last, o) }}
	case strings.Contains(f.Command, " session digest"):
		return cli.Outcome{Terminal: func(o cli.TerminalOptions) string {
			return terminalSessionDigest(strings.Repeat("0123456789abcdef", 4), o)
		}}
	case strings.Contains(f.Command, " session list"):
		bindings := []proxy.SessionBindingDocument{{SessionDigest: strings.Repeat("0123456789abcdef", 4), Pool: "work"}}
		if strings.Contains(scenario, "Empty") {
			bindings = nil
		}
		return cli.Outcome{Terminal: func(o cli.TerminalOptions) string { return terminalSessionList(bindings, o) }}
	case strings.Contains(f.Command, " session show"):
		binding := proxy.SessionBindingDocument{SessionDigest: strings.Repeat("0123456789abcdef", 4), Pool: "work"}
		return cli.Outcome{Terminal: func(o cli.TerminalOptions) string { return terminalSessionBinding(binding, true, o) }}
	default:
		data := routingFixturePolicy(f)
		kind := "policy"
		if strings.Contains(f.Command, " pool ") {
			kind = "pool"
		}
		if strings.Contains(f.Command, " session ") {
			kind = "session"
		}
		offline := strings.Contains(scenario, "Offline")
		return cli.Outcome{Terminal: func(o cli.TerminalOptions) string { return terminalPolicy(data, kind, last, offline, o) }}
	}
}

func routingFixtureReserve(f terminalRoutingFixture) v2ReserveStatus {
	str := func(s string) *string { return &s }
	pct := func(n float64) *float64 { return &n }
	status := v2ReserveStatus{Configured: true, Window: str("7d"), Percent: pct(2.05), AccountKey: str("acct-alex"), Email: str("alex@example.test"), Enabled: true, RemainingPct: pct(12.5), ResetAt: str("2026-09-23T17:45:00Z"), ObservedAt: str("2026-09-23T15:29:55Z")}
	status.Windows = []v2ReserveWindow{{Selector: "7d", RemainingPct: 12, RemainingPctExact: pct(12.5), ResetAt: status.ResetAt}, {Selector: "7d:gpt-reserve", RemainingPct: 100, ResetAt: str("2026-09-30T15:30:00Z")}}
	switch {
	case strings.Contains(f.Scenario, "Threshold reached"):
		status.Blocked, status.Reason, status.RemainingPct = true, str("reserve_reached"), pct(2)
		status.Windows[0].RemainingPctExact = pct(2)
	case strings.Contains(f.Scenario, "Unavailable observation"):
		status.Blocked, status.Reason, status.RemainingPct, status.ResetAt, status.ObservedAt, status.Windows = true, str("usage_stale"), nil, nil, nil, nil
	case strings.Contains(f.Scenario, "unconfigured") || strings.HasSuffix(f.Command, " clear"):
		status.Configured, status.Window, status.Percent, status.Enabled = false, nil, nil, false
		status.RemainingPct, status.ResetAt, status.ObservedAt = nil, nil, nil
	case strings.HasSuffix(f.Command, " disable"):
		status.Enabled, status.Reason = false, str("disabled_until_reset")
	case strings.Contains(f.Scenario, "no observed windows"):
		status.Windows = nil
	}
	return status
}

func routingFixturePolicy(f terminalRoutingFixture) v2PolicyDocument {
	policy := v2PolicyDocument{SchemaVersion: 1, AuthorityGeneration: 12, RoutingGeneration: 18, EffectiveGeneration: 18,
		Pools:           []v2PolicyPool{{Name: "work", Value: 10, Members: []codex.AccountKey{"acct-alex", "acct-sam"}}},
		SessionBindings: []proxy.SessionBindingDocument{{SessionDigest: strings.Repeat("0123456789abcdef", 4), Pool: "work"}}}
	if strings.Contains(f.Scenario, "without collections") {
		policy.Pools, policy.SessionBindings = nil, nil
	}
	if strings.Contains(f.Scenario, "Offline") || strings.Contains(f.Scenario, "retained capability") {
		policy.EffectiveGeneration = 17
	}
	if strings.Contains(f.Scenario, "retained capability") {
		observed := time.Date(2026, 9, 23, 15, 25, 0, 125000000, time.UTC)
		expires := time.Date(2026, 9, 23, 17, 45, 0, 0, time.UTC)
		policy.CapabilityEvidence = []proxy.CapabilityEvidenceV1{{AccountKey: "acct-alex", State: proxy.CapabilitySupported}, {AccountKey: "acct-sam", State: proxy.CapabilityUnknown}}
		pool := "work"
		policy.CapabilityPool = &pool
		policy.CapabilityPredicates = []proxy.CapabilityPredicateCoreV1{{SchemaVersion: 1, Capability: "program-access", ProductSurface: "codex", AccessPath: "responses", AuthMode: "oauth", RequestedModel: "gpt-5.4", EffectiveModel: "*"}}
		policy.CapabilityRoutingEvidence = []v2PolicyEvidence{{SchemaVersion: 1, AccountKey: "acct-alex", AccountKeyHMAC: "89abcdef0123456789abcdef0123456789abcdef0123456789abcdef01234567", Workspace: "example-workspace", Capability: "program-access", ProductSurface: "codex", AccessPath: "responses", AuthMode: "oauth", RequestedModel: "gpt-5.4", EffectiveModel: "gpt-5.4", Source: "authenticated-probe", State: proxy.CapabilityEvidenceEligible, ObservedAt: observed, ExpiresAt: &expires, RoutingGeneration: 18, Authenticated: true}}
		policy.Delegations = []proxy.CallerDelegationV1{{Caller: "example-runner", Accounts: []codex.AccountKey{"acct-alex", "acct-sam"}, ExpiresAt: time.Date(2026, 9, 23, 17, 0, 0, 0, time.UTC)}}
	}
	if strings.Contains(f.Command, " pool rename") {
		policy.Pools[0].Name = "primary"
		policy.SessionBindings[0].Pool = "primary"
	}
	if strings.Contains(f.Command, " pool value") {
		policy.Pools[0].Value = 20
	}
	if strings.Contains(f.Command, " session unbind") {
		policy.SessionBindings = nil
	}
	return policy
}

func TestTerminalRoutingRolesAndColourGates(t *testing.T) {
	o := terminalFixtureOptions(100)
	status := routingFixtureReserve(terminalRoutingFixture{terminalFixtureRegistration: terminalFixtureRegistration{Command: "cq codex proxy reserve status", Scenario: "Threshold reached · valid observation"}})
	got := terminalReserve(status, "status", o)
	for _, styled := range []string{"\x1b[1mCodex reserve\x1b[0m", "\x1b[36macct-alex\x1b[0m", "\x1b[1;34m2.05% of full window\x1b[0m", "\x1b[35min 2h 15m", "\x1b[33mexcluded by reserve\x1b[0m"} {
		if !strings.Contains(got, styled) {
			t.Errorf("missing semantic role %q", styled)
		}
	}
	status.Blocked = false
	got = terminalReserve(status, "status", o)
	for _, neutral := range []string{"\x1b[36myes\x1b[0m", "\x1b[36mnot excluded by reserve\x1b[0m"} {
		if !strings.Contains(got, neutral) {
			t.Errorf("missing neutral reserve state %q", neutral)
		}
	}
	status.Enabled = false
	if !strings.Contains(terminalReserve(status, "status", o), "\x1b[36mno\x1b[0m") {
		t.Error("disabled reserve state is not neutral")
	}
	policy := routingFixturePolicy(terminalRoutingFixture{terminalFixtureRegistration: terminalFixtureRegistration{Command: "cq codex proxy policy show", Scenario: "Success · retained capability evidence"}})
	policyText := terminalPolicy(policy, "policy", "show", false, o)
	for _, identity := range []string{"\x1b[36m12\x1b[0m", "\x1b[36m18\x1b[0m", "\x1b[36m1\x1b[0m"} {
		if !strings.Contains(policyText, identity) {
			t.Errorf("missing cyan generation/schema %q", identity)
		}
	}
	if !strings.Contains(policyText, "\x1b[1;34m10\x1b[0m") {
		t.Error("pool capacity value lost quantitative role")
	}
	if !strings.Contains(terminalLease(proxy.CodexLeaseInvalidationResult{JournalGeneration: 42}, o), "\x1b[36m42\x1b[0m") {
		t.Error("journal generation is not cyan")
	}
	if strings.Count(ansi.Strip(policyText), "Expires ") != 2 || !strings.Contains(policyText, "Expires \x1b[35m") || strings.Contains(policyText, "Expires \x1b[33munknown\x1b[0m") {
		t.Error("present expiry did not render as a time")
	}
	policy.CapabilityRoutingEvidence[0].ExpiresAt = nil
	policyText = terminalPolicy(policy, "policy", "show", false, o)
	if !strings.Contains(policyText, "Expires \x1b[33munknown\x1b[0m") || strings.Count(ansi.Strip(policyText), "Expires ") != 2 {
		t.Error("missing expiry did not render unknown exactly once alongside delegation expiry")
	}
	for _, disabled := range []cli.TerminalOptions{{Width: 100, Now: o.Now, Location: o.Location}, {Width: 0, Colour: true, Now: o.Now, Location: o.Location}} {
		if strings.Contains(terminalReserve(status, "status", disabled), "\x1b[") {
			t.Fatal("colour leaked when terminal colour is disabled")
		}
	}
	claude := routingFixtureOutcome(terminalRoutingFixture{terminalFixtureRegistration: terminalFixtureRegistration{Command: "cq claude proxy pin set"}, Exit: 7, Code: "routing_timeout", Message: "Routing operation timed out; inspect current state before retrying a mutation."})
	codex := routingFixtureOutcome(terminalRoutingFixture{terminalFixtureRegistration: terminalFixtureRegistration{Command: "cq codex proxy pin set"}, Exit: 7, Code: "routing_timeout", Message: claude.Errors[0].Message, Result: "unconfirmed"})
	var stdout, stderr bytes.Buffer
	for _, tc := range []struct {
		path, marker, result string
		outcome              cli.Outcome
	}{{"claude proxy pin set", "Warning", "Inspect saved state", claude}, {"codex proxy pin set", "Error", "Result: unconfirmed", codex}} {
		stdout.Reset()
		stderr.Reset()
		session := &cli.Session{Out: &stdout, Err: &stderr, OutTerminal: o, ErrTerminal: o}
		account := "acct-alex"
		if strings.HasPrefix(tc.path, "claude") {
			account = "alex@example.test"
		}
		args := append(strings.Fields(tc.path), account)
		exit := cli.Run(context.Background(), args, session, func(string) (cli.Handler, bool) {
			return func(context.Context, cli.Invocation, *cli.Session) cli.Outcome { return tc.outcome }, true
		})
		wantStdout := strings.HasPrefix(tc.path, "claude")
		if exit != 7 || (stdout.Len() != 0) != wantStdout || !strings.Contains(ansi.Strip(stderr.String()), tc.marker) || !strings.Contains(ansi.Strip(stderr.String()), tc.result) {
			t.Fatalf("%s: exit=%d stdout=%q stderr=%q", tc.path, exit, &stdout, &stderr)
		}
		if wantStdout && (!strings.Contains(ansi.Strip(stdout.String()), "Account          alex@example.test") || !strings.Contains(ansi.Strip(stdout.String()), "Live application unconfirmed")) {
			t.Fatalf("known Claude save receipt lost: %q", &stdout)
		}
	}
}

func TestTerminalReserveFreshnessBoundary(t *testing.T) {
	rig := newV2ReserveRig(t, "reserve-equality")
	check := func(age time.Duration) v2ReserveStatus {
		t.Helper()
		*rig.now = rig.now.Add(age)
		var stdout, stderr bytes.Buffer
		o := terminalFixtureOptions(100)
		exit := cli.Run(context.Background(), []string{"codex", "proxy", "reserve", "status"}, &cli.Session{Out: &stdout, Err: &stderr, OutTerminal: o, ErrTerminal: o}, rig.fixture.Lookup)
		if exit != 0 || stderr.Len() != 0 {
			t.Fatalf("status exit=%d stderr=%q", exit, &stderr)
		}
		state := v2ReserveResource(rig.reserve.Status())
		if !strings.Contains(ansi.Strip(stdout.String()), "Codex reserve") {
			t.Fatal("missing terminal status")
		}
		return state
	}
	if state := check(5 * time.Second); state.Reason == nil || *state.Reason != "reserve_reached" {
		t.Fatalf("five-second observation unexpectedly stale: %+v", state)
	}
	if state := check(11 * time.Second); state.Reason == nil || *state.Reason != "usage_stale" {
		t.Fatalf("observation past existing freshness limit did not go stale: %+v", state)
	}
}

func TestTerminalRoutingAdapterPreservesPlainAndJSON(t *testing.T) {
	cfg := &proxy.Config{CodexWindowPriming: proxy.CodexWindowPrimingConfig{ModelOverrides: map[string]string{"gpt-reserve": "gpt-5.4"}}}
	loads, saves := 0, 0
	deps := v2SelectionDependencies{LoadConfig: func() (*proxy.Config, error) { loads++; return cfg, nil }, SaveConfig: func(*proxy.Config) error { saves++; return nil }}
	lookup := func(path string) (cli.Handler, bool) {
		_, ok := lookupV2Selection(path)
		return func(ctx context.Context, inv cli.Invocation, session *cli.Session) cli.Outcome {
			return handleV2SelectionWithDependencies(ctx, inv, session, deps)
		}, ok
	}
	args := []string{"codex", "proxy", "prime", "enable"}
	var terminalOut, plainOut, jsonOut, stderr bytes.Buffer
	o := terminalFixtureOptions(100)
	if exit := cli.Run(context.Background(), args, &cli.Session{Out: &terminalOut, Err: &stderr, OutTerminal: o, ErrTerminal: o}, lookup); exit != 0 {
		t.Fatalf("terminal exit %d: %s", exit, &stderr)
	}
	assertTerminalFixture(t, "text/cq-codex-proxy-prime-enable/02-success.txt", terminalOut.String())
	if exit := cli.Run(context.Background(), args, &cli.Session{Out: &plainOut, Err: &stderr}, lookup); exit != 0 {
		t.Fatalf("plain exit %d: %s", exit, &stderr)
	}
	if plainOut.String() != "Codex window priming: enabled\nModel overrides: 1\nRestart required: true\n" {
		t.Fatalf("plain output changed: %q", &plainOut)
	}
	if exit := cli.Run(context.Background(), append(append([]string{}, args...), "--json"), &cli.Session{Out: &jsonOut, Err: &stderr, OutTerminal: o, ErrTerminal: o}, lookup); exit != 0 {
		t.Fatalf("JSON exit %d: %s", exit, &stderr)
	}
	var envelope struct {
		Data struct {
			Enabled        bool              `json:"enabled"`
			ModelOverrides map[string]string `json:"model_overrides"`
		} `json:"data"`
	}
	if err := json.Unmarshal(jsonOut.Bytes(), &envelope); err != nil || !envelope.Data.Enabled || envelope.Data.ModelOverrides["gpt-reserve"] != "gpt-5.4" {
		t.Fatalf("JSON contract changed: %s: %v", &jsonOut, err)
	}
	if loads != 3 || saves != 3 {
		t.Fatalf("presentation changed operation counts: loads=%d saves=%d", loads, saves)
	}
}

func TestTerminalReserveUnknownReasons(t *testing.T) {
	for code, label := range map[string]string{"system_account_unavailable": "system account unavailable", "reserve_state_write_failed": "reserve state could not be saved", "usage_unsettled": "usage observation unsettled"} {
		state := routingFixtureReserve(terminalRoutingFixture{})
		state.Reason = &code
		state.Blocked = true
		got := terminalReserve(state, "status", terminalFixtureOptions(100))
		if !strings.Contains(ansi.Strip(got), label+" ("+code+")") || !strings.Contains(got, "\x1b[33m"+label) {
			t.Errorf("reason %s did not remain visible and attentive", code)
		}
	}
}
