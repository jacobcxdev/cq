package main

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func cacheProfileLogPath(roots userdirs.Roots) string {
	return filepath.Join(roots.Logs, "cache-profiles", "requests.jsonl")
}

func openDefaultCacheProfiler(cfg *proxy.Config, roots userdirs.Roots) (*proxy.CacheProfiler, error) {
	if cfg == nil || cfg.CacheProfilingDisabled {
		return nil, nil
	}
	path := cacheProfileLogPath(roots)
	if err := fsutil.EnsureSecureDirectory(fsutil.OSFileSystem{}, filepath.Dir(path)); err != nil {
		return nil, err
	}
	return proxy.OpenCacheProfiler(path)
}

type proxyCacheOptions struct {
	since   time.Duration
	session string
	path    string
	json    bool
	events  bool
}

type cacheProfileTotals struct {
	Requests                       int      `json:"requests"`
	KnownUsage                     int      `json:"known_usage_requests"`
	UnknownUsage                   int      `json:"unknown_usage_requests"`
	InputTokens                    int64    `json:"input_tokens"`
	CacheMeasuredInputTokens       int64    `json:"cache_measured_input_tokens"`
	CacheMeasuredCachedInputTokens int64    `json:"cache_measured_cached_input_tokens"`
	CachedInputTokens              int64    `json:"cached_input_tokens"`
	CacheWriteInputTokens          int64    `json:"cache_write_input_tokens"`
	OutputTokens                   int64    `json:"output_tokens"`
	HitRate                        *float64 `json:"cache_hit_rate,omitempty"`
	PrefixChanges                  int      `json:"prefix_changes"`
	CachedTokenDecreases           int      `json:"cached_token_decreases"`
}

type cacheProfileGroup struct {
	Protocol string `json:"protocol"`
	Route    string `json:"route"`
	Session  string `json:"session_hash,omitempty"`
	Account  string `json:"account_hash,omitempty"`
	Model    string `json:"model_hash,omitempty"`
	cacheProfileTotals
}

type cacheProfileReport struct {
	SchemaVersion int       `json:"schema_version"`
	Log           string    `json:"log"`
	Since         time.Time `json:"since"`
	cacheProfileTotals
	Groups []cacheProfileGroup `json:"groups"`
}

func parseProxyCacheOptions(args []string) (proxyCacheOptions, error) {
	opts := proxyCacheOptions{since: 24 * time.Hour}
	seen := make(map[string]bool)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if seen[arg] {
			return opts, fmt.Errorf("proxy cache: repeated %s", arg)
		}
		seen[arg] = true
		switch arg {
		case "--json":
			opts.json = true
		case "--events":
			opts.events = true
		case "--since", "--session", "--log":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("proxy cache: %s requires a value", arg)
			}
			i++
			switch arg {
			case "--since":
				value, err := time.ParseDuration(args[i])
				if err != nil || value <= 0 {
					return opts, errors.New("proxy cache: invalid --since")
				}
				opts.since = value
			case "--session":
				value, err := hex.DecodeString(args[i])
				if err != nil || len(value) != 32 {
					return opts, errors.New("proxy cache: --session requires a reported 64-character session hash")
				}
				opts.session = args[i]
			case "--log":
				if !filepath.IsAbs(args[i]) || filepath.Clean(args[i]) != args[i] {
					return opts, errors.New("proxy cache: --log requires a clean absolute path")
				}
				opts.path = args[i]
			}
		default:
			return opts, fmt.Errorf("proxy cache: unknown argument %q", arg)
		}
	}
	return opts, nil
}

func runProxyCache(args []string, output io.Writer) error {
	opts, err := parseProxyCacheOptions(args)
	if err != nil {
		return err
	}
	if opts.path == "" {
		roots, err := userdirs.Default()
		if err != nil {
			return err
		}
		opts.path = cacheProfileLogPath(roots)
	}
	return writeProxyCacheReport(output, opts, time.Now())
}

func writeProxyCacheReport(output io.Writer, opts proxyCacheOptions, now time.Time) error {
	if output == nil {
		return errors.New("proxy cache: output unavailable")
	}
	report := cacheProfileReport{SchemaVersion: 1, Log: opts.path, Since: now.Add(-opts.since), Groups: []cacheProfileGroup{}}
	groups := make(map[string]*cacheProfileGroup)
	for _, path := range proxy.DiagnosticsLogPaths(opts.path) {
		file, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("proxy cache: open profile log: %w", err)
		}
		err = readCacheProfileEvents(file, func(event proxy.CacheProfileEvent) error {
			if event.Time.Before(report.Since) || opts.session != "" && event.SessionHash != opts.session {
				return nil
			}
			if opts.events {
				return json.NewEncoder(output).Encode(event)
			}
			key := string(event.Protocol) + "/" + event.Route + "/" + event.SessionHash + "/" + event.AccountHash + "/" + event.ModelHash
			group := groups[key]
			if group == nil {
				group = &cacheProfileGroup{Protocol: string(event.Protocol), Route: event.Route, Session: event.SessionHash, Account: event.AccountHash, Model: event.ModelHash}
				groups[key] = group
			}
			if err := group.add(event); err != nil {
				return err
			}
			return report.add(event)
		})
		err = errors.Join(err, file.Close())
		if err != nil {
			return err
		}
	}
	if opts.events {
		return nil
	}
	for _, group := range groups {
		group.setHitRate()
		report.Groups = append(report.Groups, *group)
	}
	sort.Slice(report.Groups, func(i, j int) bool {
		a, b := report.Groups[i], report.Groups[j]
		if a.InputTokens != b.InputTokens {
			return a.InputTokens > b.InputTokens
		}
		return a.Protocol+a.Route+a.Session+a.Account+a.Model < b.Protocol+b.Route+b.Session+b.Account+b.Model
	})
	report.setHitRate()
	if opts.json {
		return json.NewEncoder(output).Encode(report)
	}
	if report.Requests == 0 {
		_, err := fmt.Fprintf(output, "No cache profiles recorded in selected period. Log: %s\n", opts.path)
		return err
	}
	_, err := fmt.Fprintf(output, "Cache profiles: %d requests; %d known cache counters, %d unknown\nReported input: %d; cached: %d; cache writes: %d; output: %d\n", report.Requests, report.KnownUsage, report.UnknownUsage, report.InputTokens, report.CachedInputTokens, report.CacheWriteInputTokens, report.OutputTokens)
	if err != nil {
		return err
	}
	if report.HitRate != nil {
		if _, err := fmt.Fprintf(output, "Provider-reported cache hit rate (complete counters): %.2f%% (%d/%d)\n", *report.HitRate*100, report.CacheMeasuredCachedInputTokens, report.CacheMeasuredInputTokens); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(output, "Prefix changes: %d; observed cached-token decreases: %d\nPrefix comparisons measure bytes, not provider cache eligibility. Inspect with --events --json.\n", report.PrefixChanges, report.CachedTokenDecreases)
	return err
}

func (totals *cacheProfileTotals) add(event proxy.CacheProfileEvent) error {
	// Known describes cache-counter availability. Input/output may still be
	// reported when a provider omits its cached-input counter.
	values := []struct {
		target *int64
		delta  int64
	}{
		{&totals.InputTokens, event.Usage.InputTokens},
		{&totals.OutputTokens, event.Usage.OutputTokens},
		{&totals.CachedInputTokens, event.Usage.CachedInputTokens},
		{&totals.CacheWriteInputTokens, event.Usage.CacheWriteInputTokens},
	}
	if event.Usage.Known {
		values = append(values, struct {
			target *int64
			delta  int64
		}{&totals.CacheMeasuredInputTokens, event.Usage.InputTokens}, struct {
			target *int64
			delta  int64
		}{&totals.CacheMeasuredCachedInputTokens, event.Usage.CachedInputTokens})
	}
	for _, value := range values {
		if value.delta < 0 || *value.target > math.MaxInt64-value.delta {
			return errors.New("proxy cache: aggregate counter overflow")
		}
	}
	for _, value := range values {
		*value.target += value.delta
	}
	totals.Requests++
	if event.Usage.Known {
		totals.KnownUsage++
	} else {
		totals.UnknownUsage++
	}
	if event.Comparison != nil && !event.Comparison.PrefixStable && event.Snapshot.Complete {
		totals.PrefixChanges++
	}
	if event.CachedTokensDecreased {
		totals.CachedTokenDecreases++
	}
	return nil
}

func (totals *cacheProfileTotals) setHitRate() {
	if totals.CacheMeasuredInputTokens > 0 {
		rate := float64(totals.CacheMeasuredCachedInputTokens) / float64(totals.CacheMeasuredInputTokens)
		totals.HitRate = &rate
	}
}

func readCacheProfileEvents(reader io.Reader, visit func(proxy.CacheProfileEvent) error) error {
	buffer := bufio.NewReaderSize(reader, 64<<10)
	for {
		var line []byte
		for {
			part, err := buffer.ReadSlice('\n')
			if len(line)+len(part) > 2<<20 {
				return errors.New("proxy cache: profile record exceeds limit")
			}
			line = append(line, part...)
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if errors.Is(err, io.EOF) {
				// An incomplete final line may still be in flight. It is never
				// counted as a request with zero or missing usage.
				return nil
			}
			if err != nil {
				return err
			}
			break
		}
		var event proxy.CacheProfileEvent
		if err := json.Unmarshal(line, &event); err != nil {
			return errors.New("proxy cache: invalid profile record")
		}
		if event.EventType != "cache_profile" || event.SchemaVersion != 1 || event.Time.IsZero() {
			return errors.New("proxy cache: unsupported profile record")
		}
		u := event.Usage
		if u.InputTokens < 0 || u.CachedInputTokens < 0 || u.CacheWriteInputTokens < 0 || u.OutputTokens < 0 || u.CachedInputTokens > u.InputTokens || u.CacheWriteInputTokens > u.InputTokens-u.CachedInputTokens {
			return errors.New("proxy cache: invalid usage counters")
		}
		if err := visit(event); err != nil {
			return err
		}
	}
}
