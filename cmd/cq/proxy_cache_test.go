package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/cacheprofile"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func TestProxyCacheIsReadOnlyWithoutProxyContact(t *testing.T) {
	for _, args := range [][]string{{"proxy", "cache"}, {"proxy", "cache", "--since", "1h", "--json"}} {
		authority, err := ClassifyProxyCommand(args)
		if err != nil || authority.Terminating || authority.Row != "proxy_cache" {
			t.Fatalf("cache inspection authority = %+v, %v", authority, err)
		}
	}
}

func TestProxyCacheReportUsesWeightedKnownUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.jsonl")
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	events := []proxy.CacheProfileEvent{
		{SchemaVersion: 1, EventType: "cache_profile", Time: now.Add(-time.Minute), Protocol: cacheprofile.Messages, Route: "zcode", Usage: cacheprofile.Usage{Known: true, InputTokens: 100, CachedInputTokens: 95, OutputTokens: 10}},
		{SchemaVersion: 1, EventType: "cache_profile", Time: now.Add(-time.Second), Protocol: cacheprofile.Messages, Route: "zcode", Usage: cacheprofile.Usage{Known: true, InputTokens: 10000, CachedInputTokens: 5000, OutputTokens: 20}, CachedTokensDecreased: true},
		{SchemaVersion: 1, EventType: "cache_profile", Time: now, Protocol: cacheprofile.Responses, Route: "native", Usage: cacheprofile.Usage{Known: false, InputTokens: 200, CachedInputTokens: 20, CacheWriteInputTokens: 5, OutputTokens: 7}},
	}
	var data bytes.Buffer
	for _, event := range events {
		if err := json.NewEncoder(&data).Encode(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := writeProxyCacheReport(&output, proxyCacheOptions{path: path, since: time.Hour, json: true}, now); err != nil {
		t.Fatal(err)
	}
	var report cacheProfileReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Requests != 3 || report.KnownUsage != 2 || report.UnknownUsage != 1 || report.InputTokens != 10300 || report.CacheMeasuredInputTokens != 10100 || report.CachedInputTokens != 5115 || report.CacheMeasuredCachedInputTokens != 5095 || report.OutputTokens != 37 || report.CacheWriteInputTokens != 5 || report.CachedTokenDecreases != 1 {
		t.Fatalf("report = %+v", report)
	}
	if report.HitRate == nil || *report.HitRate != float64(5095)/10100 {
		t.Fatalf("hit rate = %v", report.HitRate)
	}
}

func TestProxyCacheFiltersRotatedRecordsAndKeepsUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.jsonl")
	now := time.Now().UTC()
	for index, event := range []proxy.CacheProfileEvent{
		{SchemaVersion: 1, EventType: "cache_profile", Time: now.Add(-2 * time.Hour), SessionHash: strings.Repeat("a", 64)},
		{SchemaVersion: 1, EventType: "cache_profile", Time: now, SessionHash: strings.Repeat("b", 64)},
	} {
		data, _ := json.Marshal(event)
		name := path
		if index == 0 {
			name += ".1"
		}
		if err := os.WriteFile(name, append(data, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	if err := writeProxyCacheReport(&output, proxyCacheOptions{path: path, since: time.Hour, session: strings.Repeat("b", 64), json: true}, now); err != nil {
		t.Fatal(err)
	}
	var report cacheProfileReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Requests != 1 || report.UnknownUsage != 1 || report.HitRate != nil {
		t.Fatalf("report = %+v", report)
	}
}

func TestProxyCacheRejectsInvalidUsageAndCompleteMalformedRecords(t *testing.T) {
	for _, line := range []string{
		`{"schema_version":1,"event_type":"cache_profile","time":"2026-11-08T12:00:00Z","usage":{"known":true,"input_tokens":1,"cached_input_tokens":2}}` + "\n",
		`{"schema_version":1,"event_type":"cache_profile","time":}` + "\n",
		`{"schema_version":1` + "\n",
	} {
		if err := readCacheProfileEvents(strings.NewReader(line), func(proxy.CacheProfileEvent) error { return nil }); err == nil {
			t.Fatalf("invalid profile accepted: %s", line)
		}
	}
}

func TestProxyCacheOptionsRejectInvalidInput(t *testing.T) {
	for _, args := range [][]string{{"--since", "0s"}, {"--since", "-1h"}, {"--session", "private-thread-id"}, {"--log", "relative.jsonl"}, {"--json", "--json"}, {"--since", "1h", "--since", "2h"}, {"--unknown"}} {
		if _, err := parseProxyCacheOptions(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestProxyCacheRejectsAggregateOverflow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overflow.jsonl")
	now := time.Now().UTC()
	var data bytes.Buffer
	event := proxy.CacheProfileEvent{SchemaVersion: 1, EventType: "cache_profile", Time: now, Usage: cacheprofile.Usage{Known: true, InputTokens: 9223372036854775807, CachedInputTokens: 9223372036854775807}}
	for range 2 {
		if err := json.NewEncoder(&data).Encode(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := writeProxyCacheReport(&output, proxyCacheOptions{path: path, since: time.Hour, json: true}, now); err == nil || !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("overflow result = %s, %v", output.String(), err)
	}
}

func TestCacheProfilerDefaultAndExplicitOptOut(t *testing.T) {
	root := t.TempDir()
	roots := userdirs.Roots{Logs: filepath.Join(root, "logs")}
	profiler, err := openDefaultCacheProfiler(&proxy.Config{CacheProfilingDisabled: true}, roots)
	if err != nil || profiler != nil {
		t.Fatalf("disabled profiler = %v, %v", profiler, err)
	}
	if _, err := os.Stat(roots.Logs); !os.IsNotExist(err) {
		t.Fatalf("opt-out created state: %v", err)
	}
	if runtime.GOOS == "windows" {
		// Windows secure creation is bound to the real user's CQ AppData
		// roots. The native installation fixture owns that qualification;
		// this arbitrary TempDir must not weaken the production boundary.
		return
	}
	profiler, err = openDefaultCacheProfiler(&proxy.Config{}, roots)
	if err != nil || profiler == nil {
		t.Fatalf("default profiler = %v, %v", profiler, err)
	}
	if err = profiler.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(cacheProfileLogPath(roots)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("private profile = %v, %v", info, err)
	}
}

func TestProxyCacheHelpDescribesObservedAndStructuralReuse(t *testing.T) {
	text, ok := manualHelp([]string{"proxy", "cache"})
	if !ok || !strings.Contains(text, "provider-reported") || !strings.Contains(text, "prefix") {
		t.Fatalf("cache help = %q, %v", text, ok)
	}
}
