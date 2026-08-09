package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

// ErrCodexRateLimitInvalid marks rate-limit telemetry that cannot safely
// produce an authoritative capacity observation.
var ErrCodexRateLimitInvalid = errors.New("invalid Codex rate-limit telemetry")

const (
	codexRateLimitMaxHeaderBytes  = 64 << 10
	codexRateLimitMaxHeaderFields = 128
	codexRateLimitMaxJSONDepth    = 64
	codexRateLimitMaxJSONFields   = 128
)

// CodexRateLimitObservation is a source-neutral capacity observation. The
// caller owns source ordering and connection-generation assignment.
type CodexRateLimitObservation struct {
	Bucket       CapacityBucket
	RemainingPct int
	ResetAt      time.Time
}

// CodexCapacityFactSink accepts capacity facts without exposing selector or
// lease policy to response observation.
type CodexCapacityFactSink interface {
	Observe(CapacityFact) bool
}

// codexRateLimitProducer owns the ordering stream for one upstream HTTP
// response or WebSocket connection.
type codexRateLimitProducer struct {
	sink                    CodexCapacityFactSink
	stream                  *CodexCapacityObservationStream
	account                 codex.AccountKey
	now                     func() time.Time
	liveEventsAuthoritative bool

	mu sync.Mutex
}

func newCodexRateLimitProducer(
	sink CodexCapacityFactSink,
	stream *CodexCapacityObservationStream,
	account codex.AccountKey,
	now func() time.Time,
	liveEventsAuthoritative bool,
) *codexRateLimitProducer {
	if sink == nil || stream == nil || account == "" {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	return &codexRateLimitProducer{
		sink:                    sink,
		stream:                  stream,
		account:                 account,
		now:                     now,
		liveEventsAuthoritative: liveEventsAuthoritative,
	}
}

func (producer *codexRateLimitProducer) ObserveHeaders(header http.Header) error {
	if producer == nil {
		return nil
	}
	observations, err := ParseCodexRateLimitHeaders(header)
	if err != nil {
		return err
	}
	producer.observe(observations, CapacitySourceHTTPHeaders)
	return nil
}

func (producer *codexRateLimitProducer) ObserveEvent(payload []byte) error {
	if producer == nil || !producer.liveEventsAuthoritative {
		return nil
	}
	observations, err := ParseCodexRateLimitEvent(payload)
	if err != nil {
		return err
	}
	producer.observe(observations, CapacitySourceLiveRateLimits)
	return nil
}

func (producer *codexRateLimitProducer) ObserveHardLimit(bucket CapacityBucket, resetAt time.Time) {
	if producer == nil {
		return
	}
	producer.mu.Lock()
	defer producer.mu.Unlock()
	producer.sink.Observe(producer.stream.Stamp(CapacityFact{
		AccountKey:   producer.account,
		Bucket:       bucket,
		RemainingPct: 0,
		Source:       CapacitySourceHardLimit,
		ObservedAt:   producer.now(),
		ResetAt:      resetAt,
		Confidence:   CapacityConfidenceAuthoritative,
	}))
}

func (producer *codexRateLimitProducer) observe(observations []CodexRateLimitObservation, source CapacitySource) {
	producer.mu.Lock()
	defer producer.mu.Unlock()
	observedAt := producer.now()
	for _, observation := range observations {
		producer.sink.Observe(producer.stream.Stamp(observation.CapacityFact(
			producer.account,
			source,
			0,
			0,
			observedAt,
		)))
	}
}

// CapacityFact attaches caller-owned ordering metadata to an observation.
func (o CodexRateLimitObservation) CapacityFact(
	account codex.AccountKey,
	source CapacitySource,
	sequence uint64,
	connectionGeneration uint64,
	observedAt time.Time,
) CapacityFact {
	return CapacityFact{
		AccountKey:           account,
		Bucket:               o.Bucket,
		RemainingPct:         o.RemainingPct,
		Source:               source,
		Sequence:             sequence,
		ConnectionGeneration: connectionGeneration,
		ObservedAt:           observedAt,
		ResetAt:              o.ResetAt,
		Confidence:           CapacityConfidenceAuthoritative,
	}
}

type codexRateLimitWindow struct {
	usedPercent float64
	resetAt     time.Time
}

var codexRateLimitHeaderSuffixes = []string{
	"-primary-used-percent",
	"-primary-window-minutes",
	"-primary-reset-at",
	"-secondary-used-percent",
	"-secondary-window-minutes",
	"-secondary-reset-at",
	"-limit-name",
}

// ParseCodexRateLimitHeaders parses the official x-codex primary/secondary
// header families without assigning source ordering.
func ParseCodexRateLimitHeaders(headers http.Header) ([]CodexRateLimitObservation, error) {
	values := make(map[string][]string)
	totalBytes := 0
	totalFields := 0
	for name, entries := range headers {
		if len(name) > codexRateLimitMaxHeaderBytes {
			return nil, invalidCodexRateLimit("header name exceeds bound")
		}
		name = strings.ToLower(name)
		if _, relevant := codexRateLimitHeaderFamily(name); !relevant {
			continue
		}
		for _, entry := range entries {
			totalBytes += len(name) + len(entry)
			totalFields++
			if totalBytes > codexRateLimitMaxHeaderBytes || totalFields > codexRateLimitMaxHeaderFields {
				return nil, invalidCodexRateLimit("header telemetry exceeds bound")
			}
			values[name] = append(values[name], entry)
		}
	}

	families := make(map[string]struct{})
	for name := range values {
		family, ok := codexRateLimitHeaderFamily(name)
		if !ok {
			continue
		}
		if family == "" {
			return nil, invalidCodexRateLimit("empty header family")
		}
		families[family] = struct{}{}
	}

	ordered := make([]string, 0, len(families))
	if _, ok := families["codex"]; ok {
		ordered = append(ordered, "codex")
		delete(families, "codex")
	}
	additional := make([]string, 0, len(families))
	for family := range families {
		additional = append(additional, family)
	}
	sort.Strings(additional)
	ordered = append(ordered, additional...)

	observations := make([]CodexRateLimitObservation, 0, len(ordered))
	seenBuckets := make(map[CapacityBucket]struct{})
	for _, family := range ordered {
		windows, limitName, err := parseCodexRateLimitHeaderFamily(values, family)
		if err != nil {
			return nil, err
		}
		bucket, known := codexRateLimitHeaderBucket(family, limitName)
		if !known {
			continue
		}
		if _, exists := seenBuckets[bucket]; exists {
			return nil, invalidCodexRateLimit("multiple header families target bucket %q", bucket)
		}
		seenBuckets[bucket] = struct{}{}
		observations = append(observations, aggregateCodexRateLimitWindows(bucket, windows))
	}
	return observations, nil
}

func codexRateLimitHeaderFamily(name string) (string, bool) {
	if !strings.HasPrefix(name, "x-") {
		return "", false
	}
	for _, suffix := range codexRateLimitHeaderSuffixes {
		if strings.HasSuffix(name, suffix) {
			return strings.TrimSuffix(strings.TrimPrefix(name, "x-"), suffix), true
		}
	}
	return "", false
}

func parseCodexRateLimitHeaderFamily(values map[string][]string, family string) ([]codexRateLimitWindow, string, error) {
	prefix := "x-" + family
	primary, primaryPresent, err := parseCodexRateLimitHeaderWindow(values, prefix+"-primary")
	if err != nil {
		return nil, "", err
	}
	secondary, secondaryPresent, err := parseCodexRateLimitHeaderWindow(values, prefix+"-secondary")
	if err != nil {
		return nil, "", err
	}
	limitName, limitNamePresent, err := singleCodexRateLimitHeader(values, prefix+"-limit-name")
	if err != nil {
		return nil, "", err
	}
	if !primaryPresent && !secondaryPresent {
		if limitNamePresent {
			return nil, "", invalidCodexRateLimit("header family %q has a name without a window", family)
		}
		return nil, "", invalidCodexRateLimit("header family %q has no complete window", family)
	}
	windows := make([]codexRateLimitWindow, 0, 2)
	if primaryPresent {
		windows = append(windows, primary)
	}
	if secondaryPresent {
		windows = append(windows, secondary)
	}
	return windows, limitName, nil
}

func parseCodexRateLimitHeaderWindow(values map[string][]string, prefix string) (codexRateLimitWindow, bool, error) {
	usedRaw, hasUsed, err := singleCodexRateLimitHeader(values, prefix+"-used-percent")
	if err != nil {
		return codexRateLimitWindow{}, false, err
	}
	windowRaw, hasWindow, err := singleCodexRateLimitHeader(values, prefix+"-window-minutes")
	if err != nil {
		return codexRateLimitWindow{}, false, err
	}
	resetRaw, hasReset, err := singleCodexRateLimitHeader(values, prefix+"-reset-at")
	if err != nil {
		return codexRateLimitWindow{}, false, err
	}
	if !hasUsed {
		if hasWindow || hasReset {
			return codexRateLimitWindow{}, false, invalidCodexRateLimit("%s has timing without used percent", prefix)
		}
		return codexRateLimitWindow{}, false, nil
	}

	usedPercent, err := strconv.ParseFloat(usedRaw, 64)
	if err != nil || !validCodexUsedPercent(usedPercent) {
		return codexRateLimitWindow{}, false, invalidCodexRateLimit("%s used percent is invalid", prefix)
	}
	if hasWindow {
		windowMinutes, err := strconv.ParseInt(windowRaw, 10, 64)
		if err != nil || windowMinutes <= 0 {
			return codexRateLimitWindow{}, false, invalidCodexRateLimit("%s window minutes is invalid", prefix)
		}
	}
	var resetAt time.Time
	if hasReset {
		resetUnix, err := strconv.ParseInt(resetRaw, 10, 64)
		if err != nil || resetUnix <= 0 {
			return codexRateLimitWindow{}, false, invalidCodexRateLimit("%s reset time is invalid", prefix)
		}
		resetAt = time.Unix(resetUnix, 0)
	}
	return codexRateLimitWindow{usedPercent: usedPercent, resetAt: resetAt}, true, nil
}

func singleCodexRateLimitHeader(values map[string][]string, name string) (string, bool, error) {
	entries, ok := values[name]
	if !ok {
		return "", false, nil
	}
	if len(entries) != 1 {
		return "", false, invalidCodexRateLimit("header %q has duplicate values", name)
	}
	value := strings.TrimSpace(entries[0])
	if value == "" {
		return "", false, invalidCodexRateLimit("header %q is empty", name)
	}
	return value, true, nil
}

func codexRateLimitHeaderBucket(family, limitName string) (CapacityBucket, bool) {
	if family == "codex" {
		if limitName == "" || normaliseCodexRateLimitName(limitName) == "codex" {
			return CapacityBucketBase, true
		}
		return "", false
	}
	if normaliseCodexRateLimitName(limitName) == codexSparkModel {
		return CapacityBucketForModel(codexSparkModel), true
	}
	return "", false
}

// ParseCodexRateLimitEvent parses one official codex.rate_limits event without
// assigning source ordering. Other event types and valid unknown scopes emit no
// observations.
func ParseCodexRateLimitEvent(payload []byte) ([]CodexRateLimitObservation, error) {
	if len(payload) > codexSSEDefaultMaxEventBytes {
		return nil, invalidCodexRateLimit("event exceeds bound")
	}
	if err := rejectDuplicateCodexRateLimitJSONFields(payload); err != nil {
		return nil, err
	}
	var envelope struct {
		Type             string          `json:"type"`
		RateLimits       json.RawMessage `json:"rate_limits"`
		MeteredLimitName json.RawMessage `json:"metered_limit_name"`
		LimitName        json.RawMessage `json:"limit_name"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, invalidCodexRateLimit("decode event: %v", err)
	}
	if envelope.Type != "codex.rate_limits" {
		return nil, nil
	}

	meteredName, hasMeteredName, err := parseOptionalCodexRateLimitName(envelope.MeteredLimitName)
	if err != nil {
		return nil, err
	}
	legacyName, hasLegacyName, err := parseOptionalCodexRateLimitName(envelope.LimitName)
	if err != nil {
		return nil, err
	}
	if hasMeteredName && hasLegacyName {
		return nil, invalidCodexRateLimit("event has conflicting scope aliases")
	}
	limitName := "codex"
	if hasMeteredName {
		limitName = meteredName
	} else if hasLegacyName {
		limitName = legacyName
	}

	windows, err := parseCodexRateLimitEventWindows(envelope.RateLimits)
	if err != nil {
		return nil, err
	}
	if len(windows) == 0 {
		return nil, nil
	}
	bucket, known := codexRateLimitEventBucket(limitName)
	if !known {
		return nil, nil
	}
	return []CodexRateLimitObservation{aggregateCodexRateLimitWindows(bucket, windows)}, nil
}

func parseOptionalCodexRateLimitName(raw json.RawMessage) (string, bool, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", false, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false, invalidCodexRateLimit("scope name must be a string")
	}
	return strings.TrimSpace(value), true, nil
}

func parseCodexRateLimitEventWindows(raw json.RawMessage) ([]codexRateLimitWindow, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var details struct {
		Primary   json.RawMessage `json:"primary"`
		Secondary json.RawMessage `json:"secondary"`
	}
	if err := json.Unmarshal(raw, &details); err != nil {
		return nil, invalidCodexRateLimit("decode rate_limits: %v", err)
	}
	windows := make([]codexRateLimitWindow, 0, 2)
	for name, windowRaw := range map[string]json.RawMessage{
		"primary":   details.Primary,
		"secondary": details.Secondary,
	} {
		window, present, err := parseCodexRateLimitEventWindow(windowRaw, name)
		if err != nil {
			return nil, err
		}
		if present {
			windows = append(windows, window)
		}
	}
	return windows, nil
}

func parseCodexRateLimitEventWindow(raw json.RawMessage, name string) (codexRateLimitWindow, bool, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return codexRateLimitWindow{}, false, nil
	}
	var window struct {
		UsedPercent   json.RawMessage `json:"used_percent"`
		WindowMinutes json.RawMessage `json:"window_minutes"`
		ResetAt       json.RawMessage `json:"reset_at"`
	}
	if err := json.Unmarshal(raw, &window); err != nil {
		return codexRateLimitWindow{}, false, invalidCodexRateLimit("decode %s window: %v", name, err)
	}
	if len(window.UsedPercent) == 0 || bytes.Equal(bytes.TrimSpace(window.UsedPercent), []byte("null")) {
		return codexRateLimitWindow{}, false, invalidCodexRateLimit("%s window has no used percent", name)
	}
	var usedPercent float64
	if err := json.Unmarshal(window.UsedPercent, &usedPercent); err != nil || !validCodexUsedPercent(usedPercent) {
		return codexRateLimitWindow{}, false, invalidCodexRateLimit("%s window used percent is invalid", name)
	}
	if len(window.WindowMinutes) != 0 && !bytes.Equal(bytes.TrimSpace(window.WindowMinutes), []byte("null")) {
		var windowMinutes int64
		if err := json.Unmarshal(window.WindowMinutes, &windowMinutes); err != nil || windowMinutes <= 0 {
			return codexRateLimitWindow{}, false, invalidCodexRateLimit("%s window minutes is invalid", name)
		}
	}
	var resetAt time.Time
	if len(window.ResetAt) != 0 && !bytes.Equal(bytes.TrimSpace(window.ResetAt), []byte("null")) {
		var resetUnix int64
		if err := json.Unmarshal(window.ResetAt, &resetUnix); err != nil || resetUnix <= 0 {
			return codexRateLimitWindow{}, false, invalidCodexRateLimit("%s reset time is invalid", name)
		}
		resetAt = time.Unix(resetUnix, 0)
	}
	return codexRateLimitWindow{usedPercent: usedPercent, resetAt: resetAt}, true, nil
}

func codexRateLimitEventBucket(limitName string) (CapacityBucket, bool) {
	switch normaliseCodexRateLimitName(limitName) {
	case "codex":
		return CapacityBucketBase, true
	case codexSparkModel:
		return CapacityBucketForModel(codexSparkModel), true
	default:
		return "", false
	}
}

func normaliseCodexRateLimitName(name string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "_", "-")
}

func validCodexUsedPercent(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 100
}

func aggregateCodexRateLimitWindows(bucket CapacityBucket, windows []codexRateLimitWindow) CodexRateLimitObservation {
	remaining := 100
	haveLimitingWindow := false
	limitingResetKnown := false
	var resetAt time.Time
	for _, window := range windows {
		windowRemaining := int(math.Round(100 - window.usedPercent))
		switch {
		case !haveLimitingWindow || windowRemaining < remaining:
			remaining = windowRemaining
			resetAt = window.resetAt
			haveLimitingWindow = true
			limitingResetKnown = !window.resetAt.IsZero()
		case windowRemaining == remaining:
			if window.resetAt.IsZero() || !limitingResetKnown {
				resetAt = time.Time{}
				limitingResetKnown = false
			} else if window.resetAt.After(resetAt) {
				resetAt = window.resetAt
			}
		}
	}
	return CodexRateLimitObservation{Bucket: bucket, RemainingPct: remaining, ResetAt: resetAt}
}

func rejectDuplicateCodexRateLimitJSONFields(payload []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := scanCodexRateLimitJSONValue(decoder, 0); err != nil {
		return invalidCodexRateLimit("decode event: %v", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return invalidCodexRateLimit("decode event: %v", err)
	}
	return nil
}

func scanCodexRateLimitJSONValue(decoder *json.Decoder, depth int) error {
	if depth > codexRateLimitMaxJSONDepth {
		return errors.New("JSON nesting exceeds bound")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make([]string, 0)
		for decoder.More() {
			if len(seen) >= codexRateLimitMaxJSONFields {
				return errors.New("object field count exceeds bound")
			}
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			for _, previous := range seen {
				if strings.EqualFold(previous, key) {
					return errors.New("duplicate field")
				}
			}
			seen = append(seen, key)
			if err := scanCodexRateLimitJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return errors.New("unterminated object")
		}
	case '[':
		for decoder.More() {
			if err := scanCodexRateLimitJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return errors.New("unterminated array")
		}
	default:
		return fmt.Errorf("unexpected delimiter %q", delimiter)
	}
	return nil
}

func invalidCodexRateLimit(_ string, _ ...any) error {
	return ErrCodexRateLimitInvalid
}
