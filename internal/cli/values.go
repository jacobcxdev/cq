package cli

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/mail"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	integerPattern     = regexp.MustCompile(`^[0-9]+$`)
	numberPattern      = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?$`)
	digestPattern      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	idPattern          = regexp.MustCompile(`^[0-9a-f]{32}$`)
	uuidPattern        = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	clientBuildPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$`)
	timestampPattern   = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,9})?Z$`)
)

// Messages are explicit constraints, not runtime interpretations of help prose.
func validateValue(path string, p ParameterSpec, value, legacy string) string {
	if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return "valid UTF-8 without NUL required"
	}
	if p.Type == "boolean" {
		if value != "true" && value != "false" {
			return "expected true or false"
		}
		return ""
	}
	if value == "" {
		if p.Name == "account" {
			return "Account reference must not be empty"
		}
		return "must not be empty"
	}
	switch p.Type {
	case "enum":
		ok := false
		for _, v := range p.Choices {
			ok = ok || value == v
		}
		if !ok {
			return "expected one of " + strings.Join(p.Choices, ", ")
		}
	case "integer":
		if !integerPattern.MatchString(value) {
			return integerConstraint(path, p.Name)
		}
		n, e := strconv.ParseUint(value, 10, 64)
		if e != nil {
			return integerConstraint(path, p.Name)
		}
		switch p.Name {
		case "port":
			if n < 1 || n > 65535 || ((path == "proxy candidate prepare" || path == "codex proxy validate http") && n == 19280) {
				return integerConstraint(path, p.Name)
			}
		case "value":
			if n > math.MaxUint32 {
				return "Unsigned 32-bit integer"
			}
		default:
			if n > math.MaxInt64 {
				return "Integer >=0"
			}
		}
	case "number":
		n, e := strconv.ParseFloat(value, 64)
		if !numberPattern.MatchString(value) || e != nil || math.IsInf(n, 0) || math.IsNaN(n) || n <= 0 || n >= 100 {
			return "Finite number strictly greater than 0 and strictly less than 100"
		}
	case "duration":
		d, e := time.ParseDuration(value)
		lo, hi, constraint := durationBounds(path, p.Name)
		if e != nil || d < lo || d > hi {
			return constraint
		}
	case "digest":
		if !digestPattern.MatchString(value) {
			return "64 lowercase hexadecimal characters"
		}
	case "path":
		if cleanAbsoluteParameter(path, p.Name) && (!filepath.IsAbs(value) || filepath.Clean(value) != value || filepath.Dir(value) == value) {
			return "Absolute, lexically clean non-root path required"
		}
		if path == "codex proxy fixture create" && p.Name == "input" && value == "-" {
			return "Readable regular file; not stdin; nonempty path; at most 2097152 bytes"
		}
	case "account-reference":
		if strings.TrimSpace(value) == "" {
			return "Account reference must not be empty"
		}
	}
	if p.Name == "account" {
		if strings.TrimSpace(value) == "" {
			return "Account reference must not be empty"
		}
		if strings.HasPrefix(path, "claude ") && legacy != "claude_uuid" {
			trimmed := strings.TrimSpace(value)
			a, e := mail.ParseAddress(trimmed)
			if e != nil || a.Address != trimmed {
				return "Nonempty email matching exactly one known Claude account"
			}
		}
	}
	if poolParameter(path, p.Name) && (strings.TrimSpace(value) == "" || hasControls(value)) {
		return "Valid UTF-8, contains at least one non-whitespace character, contains no control characters"
	}
	switch p.Name {
	case "session-id":
		if len(value) > 4096 {
			return "UTF-8 encoded input must contain 1–4096 bytes; preserve exact bytes"
		}
	case "session":
		s := strings.TrimSpace(value)
		if n := strings.LastIndex(s, "/"); n >= 0 {
			s = s[n+1:]
		}
		if s == "" {
			return "After trimming surrounding whitespace and extracting the final slash-delimited component, selector must be nonempty"
		}
	case "credit":
		if value != strings.TrimSpace(value) {
			return "Non-empty, no surrounding whitespace"
		}
	case "id", "clone-from":
		if strings.HasPrefix(path, "models ") && (value != strings.TrimSpace(value) || hasControls(value)) {
			return "Non-empty UTF-8 string without leading or trailing whitespace or control characters"
		}
	case "client-build":
		if path == "codex proxy readiness show" || path == "codex proxy validate websocket" {
			if !clientBuildPattern.MatchString(value) {
				return "Match ^[0-9]+\\.[0-9]+\\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$; reject whitespace"
			}
		} else if hasControls(value) {
			return "Non-empty UTF-8 string; no NUL or control characters"
		}
	case "attempt-id", "operation-id":
		if !idPattern.MatchString(value) {
			return "Exactly 32 lowercase hexadecimal characters"
		}
	case "metadata-json":
		if !validFixtureMetadata(value) {
			return "When supplied, parse one FixtureMetadata object; reject trailing JSON and duplicate keys"
		}
	}
	return ""
}
func integerConstraint(path, name string) string {
	if name == "value" {
		return "Unsigned 32-bit integer"
	}
	if name == "port" {
		if path == "proxy candidate prepare" || path == "codex proxy validate http" {
			return "1..65535 except 19280"
		}
		return "Integer 1–65535"
	}
	return "Integer >=0"
}
func durationBounds(path, name string) (time.Duration, time.Duration, string) {
	if name == "since" || name == "legacy-duration" {
		return 1, time.Duration(math.MaxInt64), "Positive Go duration"
	}
	if strings.HasPrefix(path, "codex proxy credential-endpoint legacy ") {
		return time.Second, 5 * time.Minute, "Go duration syntax; 1s <= value <= 5m"
	}
	switch path {
	case "proxy candidate prepare", "proxy candidate client-safety refresh":
		return 150 * time.Second, 5 * time.Minute, "Go duration syntax; 150s <= value <= 5m"
	case "proxy candidate release activate":
		return 90 * time.Second, 2 * time.Minute, "Go duration syntax; 90s <= value <= 2m"
	case "proxy candidate receipt show", "proxy candidate status":
		return time.Second, 30 * time.Second, "Go duration syntax; 1s <= value <= 30s"
	case "proxy candidate start":
		return 30 * time.Second, 90 * time.Second, "Go duration syntax; 30s <= value <= 90s"
	case "codex proxy validate websocket":
		return 30 * time.Second, 30 * time.Minute, "Go duration; 30s <= value <=30m"
	case "codex proxy validate http", "proxy rescue enter", "proxy rescue exit", "proxy rescue status":
		return time.Second, 5 * time.Minute, "Go duration, 1s <= value <= 5m"
	case "claude account login", "codex account login":
		return time.Second, 30 * time.Minute, "Go duration syntax; 1s <= value <= 30m"
	}
	if strings.Contains(path, " account ") || strings.HasPrefix(path, "codex reset ") {
		return time.Second, 10 * time.Minute, "Go duration syntax; 1s <= value <= 10m"
	}
	if strings.HasPrefix(path, "codex proxy ") || strings.HasPrefix(path, "claude proxy ") {
		return 1, time.Duration(math.MaxInt64), "Positive Go duration"
	}
	return 1, 10 * time.Minute, "Positive Go duration, at most 10m; no bare number"
}
func cleanAbsoluteParameter(path, name string) bool {
	if strings.HasPrefix(path, "proxy candidate ") || strings.HasPrefix(path, "codex proxy credential-endpoint legacy ") {
		return true
	}
	return name == "state-dir"
}
func poolParameter(path, name string) bool {
	return strings.HasPrefix(path, "codex proxy pool ") && (name == "name" || name == "old-name" || name == "new-name") || path == "codex proxy session bind" && name == "pool"
}
func hasControls(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// parseTimestamp is shared with structured argv values; operational timestamps
// and file contents remain owned by their family adapters.
func parseTimestamp(value string) (time.Time, error) {
	if !timestampPattern.MatchString(value) {
		return time.Time{}, errors.New("expected UTC RFC3339 timestamp ending Z")
	}
	return time.Parse(time.RFC3339Nano, value)
}

func strictJSON(value string, target any) error {
	if !utf8.ValidString(value) || strings.HasPrefix(value, "\ufeff") {
		return errors.New("invalid UTF-8 JSON")
	}
	d := json.NewDecoder(strings.NewReader(value))
	d.UseNumber()
	if err := uniqueJSONValue(d); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing JSON")
	}
	d = json.NewDecoder(strings.NewReader(value))
	d.DisallowUnknownFields()
	return d.Decode(target)
}
func uniqueJSONValue(d *json.Decoder) error {
	token, e := d.Token()
	if e != nil {
		return e
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, e := d.Token()
			if e != nil {
				return e
			}
			s, ok := key.(string)
			if !ok || seen[s] {
				return errors.New("duplicate or invalid JSON key")
			}
			seen[s] = true
			if e := uniqueJSONValue(d); e != nil {
				return e
			}
		}
		token, e = d.Token()
		if e != nil || token != json.Delim('}') {
			return errors.New("invalid JSON object")
		}
	case '[':
		for d.More() {
			if e := uniqueJSONValue(d); e != nil {
				return e
			}
		}
		token, e = d.Token()
		if e != nil || token != json.Delim(']') {
			return errors.New("invalid JSON array")
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	return nil
}
func validFixtureMetadata(value string) bool {
	if len(value) > 65536 {
		return false
	}
	var raw map[string]json.RawMessage
	if strictJSON(value, &raw) != nil || raw == nil {
		return false
	}
	fields := map[string]string{}
	for k, v := range raw {
		switch k {
		case "session_id", "thread_id", "turn_id", "window_id", "request_kind", "compaction":
		default:
			return false
		}
		var s string
		if string(v) == "null" || json.Unmarshal(v, &s) != nil || strings.ContainsRune(s, 0) {
			return false
		}
		if strings.HasSuffix(k, "_id") && len(s) > 4096 {
			return false
		}
		fields[k] = s
	}
	session, thread, turn := fields["session_id"], fields["thread_id"], fields["turn_id"]
	kind, phase := fields["request_kind"], fields["compaction"]
	if kind != "compaction" {
		if _, ok := fields["compaction"]; ok {
			return false
		}
	}
	switch kind {
	case "turn":
		return session != "" && thread != "" && turn != ""
	case "prewarm":
		return session != "" && thread != "" && turn == ""
	case "compaction":
		return session != "" && thread != "" && turn != "" && (phase == "standalone_turn" || phase == "pre_turn" || phase == "mid_turn")
	case "memory":
		return true
	default:
		return false
	}
}

// catalogueConstraint supplies exact diagnostic prose only. Typed predicates
// above remain the sole executable interpretation of each parameter contract.
func catalogueConstraint(path, name, fallback string) string {
	if fallback == "Account reference must not be empty" || fallback == "valid UTF-8 without NUL required" {
		return fallback
	}
	if text, ok := valueConstraintMessages[path+"/"+name]; ok {
		return text
	}
	return fallback
}

var valueConstraintMessages = map[string]string{
	"auth refresh/providers":                                       "Reject duplicate providers and any provider other than claude or codex.",
	"check/providers":                                              "Exact lowercase IDs; duplicates invalid; one or more values when supplied.",
	"check/timeout":                                                "Positive Go duration, at most 10m; no bare number. Deadline expiry never implies rollback succeeded.",
	"claude account activate/account":                              "Non-empty if supplied.",
	"claude account activate/timeout":                              "Go duration syntax; 1s <= value <= 10m",
	"claude account list/timeout":                                  "Go duration syntax; 1s <= value <= 10m",
	"claude account login/timeout":                                 "Go duration syntax; 1s <= value <= 30m",
	"claude account remove/account":                                "Non-empty if supplied.",
	"claude account remove/timeout":                                "Go duration syntax; 1s <= value <= 10m",
	"claude proxy pin clear/timeout":                               "Positive Go duration.",
	"claude proxy pin set/account":                                 "Nonempty email matching exactly one known Claude account.",
	"claude proxy pin set/timeout":                                 "Positive Go duration.",
	"claude proxy pin show/timeout":                                "Positive Go duration.",
	"codex account activate/account":                               "Non-empty if supplied.",
	"codex account activate/timeout":                               "Go duration syntax; 1s <= value <= 10m",
	"codex account list/timeout":                                   "Go duration syntax; 1s <= value <= 10m",
	"codex account login/timeout":                                  "Go duration syntax; 1s <= value <= 30m",
	"codex account remove/account":                                 "Non-empty if supplied.",
	"codex account remove/timeout":                                 "Go duration syntax; 1s <= value <= 10m",
	"codex proxy credential-endpoint legacy activate/ticket-file":  "Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.",
	"codex proxy credential-endpoint legacy activate/timeout":      "Go duration syntax; 1s <= value <= 5m; cleanup reserve 0s; deadline cancellation must not leave an unrecorded mutation.",
	"codex proxy credential-endpoint legacy finalise/ticket-file":  "Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.",
	"codex proxy credential-endpoint legacy finalise/timeout":      "Go duration syntax; 1s <= value <= 5m; cleanup reserve 0s; deadline cancellation must not leave an unrecorded mutation.",
	"codex proxy credential-endpoint legacy inspect/timeout":       "Go duration syntax; 1s <= value <= 5m; cleanup reserve 0s; deadline cancellation must not leave an unrecorded mutation.",
	"codex proxy credential-endpoint legacy prepare/snapshot-file": "Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.",
	"codex proxy credential-endpoint legacy prepare/timeout":       "Go duration syntax; 1s <= value <= 5m; cleanup reserve 0s; deadline cancellation must not leave an unrecorded mutation.",
	"codex proxy credential-endpoint legacy resume/ticket-file":    "Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.",
	"codex proxy credential-endpoint legacy resume/timeout":        "Go duration syntax; 1s <= value <= 5m; cleanup reserve 0s; deadline cancellation must not leave an unrecorded mutation.",
	"codex proxy credential-endpoint legacy rollback/ticket-file":  "Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.",
	"codex proxy credential-endpoint legacy rollback/timeout":      "Go duration syntax; 1s <= value <= 5m; cleanup reserve 0s; deadline cancellation must not leave an unrecorded mutation.",
	"codex proxy fallback clear/timeout":                           "Positive Go duration.",
	"codex proxy fallback set/account":                             "Nonempty.",
	"codex proxy fallback set/timeout":                             "Positive Go duration.",
	"codex proxy fallback show/timeout":                            "Positive Go duration.",
	"codex proxy fixture create/input":                             "Readable regular file; not stdin; nonempty path; at most 2097152 bytes.",
	"codex proxy fixture create/output":                            "Nonempty path; must not exist; must not alias the input; parent created with mode 0700 when missing.",
	"codex proxy fixture create/content-encoding":                  "Exact enum; decoded size <= 8388608 bytes; zstd expansion <=128 times encoded size.",
	"codex proxy fixture create/metadata-json":                     "When supplied, parse one FixtureMetadata object; reject trailing JSON and duplicate keys.",
	"codex proxy lease invalidate/port":                            "Integer 1–65535.",
	"codex proxy lease invalidate/timeout":                         "Positive Go duration.",
	"codex proxy pin clear/timeout":                                "Positive Go duration.",
	"codex proxy pin set/account":                                  "Nonempty.",
	"codex proxy pin set/timeout":                                  "Positive Go duration.",
	"codex proxy pin show/timeout":                                 "Positive Go duration.",
	"codex proxy policy apply/state-dir":                           "Clean absolute non-root directory.",
	"codex proxy policy apply/port":                                "Integer 1–65535.",
	"codex proxy policy apply/timeout":                             "Positive Go duration.",
	"codex proxy policy show/state-dir":                            "Clean absolute non-root directory.",
	"codex proxy policy show/port":                                 "Integer 1–65535.",
	"codex proxy policy show/timeout":                              "Positive Go duration.",
	"codex proxy pool rename/old-name":                             "Valid UTF-8, contains at least one non-whitespace character, contains no control characters.",
	"codex proxy pool rename/new-name":                             "Valid UTF-8, contains at least one non-whitespace character, contains no control characters.",
	"codex proxy pool rename/port":                                 "Integer 1–65535.",
	"codex proxy pool rename/timeout":                              "Positive Go duration.",
	"codex proxy pool set/name":                                    "Valid UTF-8, contains at least one non-whitespace character, contains no control characters.",
	"codex proxy pool set/account":                                 "Nonempty.",
	"codex proxy pool set/value":                                   "Unsigned 32-bit integer.",
	"codex proxy pool set/port":                                    "Integer 1–65535.",
	"codex proxy pool set/timeout":                                 "Positive Go duration.",
	"codex proxy pool value/name":                                  "Valid UTF-8, contains at least one non-whitespace character, contains no control characters.",
	"codex proxy pool value/value":                                 "Unsigned 32-bit integer.",
	"codex proxy pool value/port":                                  "Integer 1–65535.",
	"codex proxy pool value/timeout":                               "Positive Go duration.",
	"codex proxy prime disable/timeout":                            "Positive Go duration.",
	"codex proxy prime enable/timeout":                             "Positive Go duration.",
	"codex proxy prime status/timeout":                             "Positive Go duration.",
	"codex proxy readiness show/client-build":                      "Match ^[0-9]+\\.[0-9]+\\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$; reject whitespace.",
	"codex proxy readiness show/state-dir":                         "If present, nonempty canonical absolute directory; reject symlink traversal.",
	"codex proxy reserve clear/port":                               "Integer 1–65535.",
	"codex proxy reserve clear/timeout":                            "Positive Go duration.",
	"codex proxy reserve disable/port":                             "Integer 1–65535.",
	"codex proxy reserve disable/timeout":                          "Positive Go duration.",
	"codex proxy reserve enable/port":                              "Integer 1–65535.",
	"codex proxy reserve enable/timeout":                           "Positive Go duration.",
	"codex proxy reserve set/window":                               "Canonicalise legacy case, surrounding spaces, and underscores exactly as released parser, then validate observed selector.",
	"codex proxy reserve set/percent":                              "Finite number strictly greater than 0 and strictly less than 100.",
	"codex proxy reserve set/port":                                 "Integer 1–65535.",
	"codex proxy reserve set/timeout":                              "Positive Go duration.",
	"codex proxy reserve status/port":                              "Integer 1–65535.",
	"codex proxy reserve status/timeout":                           "Positive Go duration.",
	"codex proxy reserve windows/port":                             "Integer 1–65535.",
	"codex proxy reserve windows/timeout":                          "Positive Go duration.",
	"codex proxy session bind/pool":                                "Valid UTF-8, contains at least one non-whitespace character, contains no control characters.",
	"codex proxy session bind/session-id":                          "UTF-8 encoded input must contain 1–4096 bytes; preserve exact bytes.",
	"codex proxy session bind/digest":                              "64 lowercase hexadecimal characters.",
	"codex proxy session bind/port":                                "Integer 1–65535.",
	"codex proxy session bind/timeout":                             "Positive Go duration.",
	"codex proxy session digest/session-id":                        "UTF-8 encoded input must contain 1–4096 bytes; preserve exact bytes.",
	"codex proxy session digest/digest":                            "64 lowercase hexadecimal characters.",
	"codex proxy session digest/port":                              "Integer 1–65535.",
	"codex proxy session digest/timeout":                           "Positive Go duration.",
	"codex proxy session list/port":                                "Integer 1–65535.",
	"codex proxy session list/timeout":                             "Positive Go duration.",
	"codex proxy session show/session-id":                          "UTF-8 encoded input must contain 1–4096 bytes; preserve exact bytes.",
	"codex proxy session show/digest":                              "64 lowercase hexadecimal characters.",
	"codex proxy session show/port":                                "Integer 1–65535.",
	"codex proxy session show/timeout":                             "Positive Go duration.",
	"codex proxy session unbind/session-id":                        "UTF-8 encoded input must contain 1–4096 bytes; preserve exact bytes.",
	"codex proxy session unbind/digest":                            "64 lowercase hexadecimal characters.",
	"codex proxy session unbind/port":                              "Integer 1–65535.",
	"codex proxy session unbind/timeout":                           "Positive Go duration.",
	"codex proxy trace/session":                                    "After trimming surrounding whitespace and extracting the final slash-delimited component, selector must be nonempty.",
	"codex proxy trace/trace":                                      "Nonempty trace ID.",
	"codex proxy trace/since":                                      "Positive Go duration.",
	"codex proxy trace/tail":                                       "Integer >=0.",
	"codex proxy trace/timeout":                                    "Positive Go duration when specified.",
	"codex proxy validate http/port":                               "1..65535 except 19280.",
	"codex proxy validate http/timeout":                            "Go duration, 1s <= value <= 5m.",
	"codex proxy validate websocket/client-build":                  "Match ^[0-9]+\\.[0-9]+\\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$; reject whitespace.",
	"codex proxy validate websocket/client-executable":             "If supplied, resolve to absolute regular executable file.",
	"codex proxy validate websocket/state-dir":                     "If present, nonempty canonical absolute directory; reject symlink traversal.",
	"codex proxy validate websocket/timeout":                       "Go duration; 30s <= value <=30m.",
	"codex reset list/account":                                     "Non-empty if supplied.",
	"codex reset list/timeout":                                     "Go duration syntax; 1s <= value <= 10m",
	"codex reset recommend/timeout":                                "Go duration syntax; 1s <= value <= 10m",
	"codex reset use/account":                                      "Non-empty if supplied.",
	"codex reset use/credit":                                       "Non-empty, no surrounding whitespace.",
	"codex reset use/timeout":                                      "Go duration syntax; 1s <= value <= 10m",
	"completion/shell":                                             "Exact lowercase enum.",
	"gemini account show/timeout":                                  "Go duration syntax; 1s <= value <= 10m",
	"help/command-path":                                            "Resolve exact full group/leaf path; unknown child fails before state access.",
	"models list/provider":                                         "Reject other values; translate legacy anthropic to claude with deprecation warning.",
	"models overlay add/provider":                                  "Reject other values; translate legacy anthropic to claude with deprecation warning.",
	"models overlay add/id":                                        "Non-empty UTF-8 string without leading or trailing whitespace or control characters.",
	"models overlay add/clone-from":                                "Non-empty UTF-8 string without leading or trailing whitespace or control characters.",
	"models overlay remove/provider":                               "Reject other values; translate legacy anthropic to claude with deprecation warning.",
	"models overlay remove/id":                                     "Non-empty UTF-8 string without leading or trailing whitespace or control characters.",
	"proxy candidate client-safety refresh/state-dir":              "Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.",
	"proxy candidate client-safety refresh/validation-run-id":      "Exactly 64 lowercase hexadecimal characters.",
	"proxy candidate client-safety refresh/timeout":                "Go duration syntax; 150s <= value <= 5m; cleanup reserve 30s; deadline cancellation must not leave an unrecorded mutation.",
	"proxy candidate prepare/state-dir":                            "Absolute, lexically clean path; not /; no symbolic link; parent owner-controlled; no existing candidate state.",
	"proxy candidate prepare/port":                                 "Integer 1..65535; must not equal 19280; must not equal any active shared or candidate listener.",
	"proxy candidate prepare/source-config":                        "Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.",
	"proxy candidate prepare/target-release-bundle":                "Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.",
	"proxy candidate prepare/release-digest":                       "Exactly 64 lowercase hexadecimal characters.",
	"proxy candidate prepare/client-build":                         "Non-empty UTF-8 string; no NUL or control characters.",
	"proxy candidate prepare/client-executable":                    "Absolute, lexically clean path to a regular executable file; no symbolic link; stable identity between inspection and opening; no group or other write permissions.",
	"proxy candidate prepare/client-registry":                      "Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.",
	"proxy candidate prepare/credential-manifest":                  "Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.",
	"proxy candidate prepare/policy-snapshot":                      "Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.",
	"proxy candidate prepare/timeout":                              "Go duration syntax; 150s <= value <= 5m; cleanup reserve 30s; deadline cancellation must not leave an unrecorded mutation.",
	"proxy candidate receipt show/state-dir":                       "Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.",
	"proxy candidate receipt show/attempt-id":                      "Exactly 32 lowercase hexadecimal characters.",
	"proxy candidate receipt show/timeout":                         "Go duration syntax; 1s <= value <= 30s; cleanup reserve 0s; deadline cancellation must not leave an unrecorded mutation.",
	"proxy candidate release activate/state-dir":                   "Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.",
	"proxy candidate release activate/release-digest":              "Exactly 64 lowercase hexadecimal characters.",
	"proxy candidate release activate/validation-run-id":           "Exactly 64 lowercase hexadecimal characters.",
	"proxy candidate release activate/timeout":                     "Go duration syntax; 90s <= value <= 2m; cleanup reserve 30s; deadline cancellation must not leave an unrecorded mutation.",
	"proxy candidate release validate/state-dir":                   "Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.",
	"proxy candidate release validate/target-release-bundle":       "Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.",
	"proxy candidate release validate/rollback-bundle":             "Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.",
	"proxy candidate release validate/rollback-receipt":            "Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.",
	"proxy candidate release validate/rollback-receipt-digest":     "Exactly 64 lowercase hexadecimal characters.",
	"proxy candidate release validate/client-build":                "Non-empty UTF-8 string; no NUL or control characters.",
	"proxy candidate release validate/client-executable":           "Absolute, lexically clean path to a regular executable file; no symbolic link; stable identity between inspection and opening; no group or other write permissions.",
	"proxy candidate release validate/validation-run-id":           "Exactly 64 lowercase hexadecimal characters.",
	"proxy candidate release validate/receipt-file":                "Absolute, lexically clean output path; not /; leaf must not exist, including as a symbolic link.",
	"proxy candidate remove/state-dir":                             "Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.",
	"proxy candidate start/state-dir":                              "Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.",
	"proxy candidate start/timeout":                                "Go duration syntax; 30s <= value <= 90s; cleanup reserve 15s; deadline cancellation must not leave an unrecorded mutation.",
	"proxy candidate status/state-dir":                             "Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.",
	"proxy candidate status/timeout":                               "Go duration syntax; 1s <= value <= 30s; cleanup reserve 0s; deadline cancellation must not leave an unrecorded mutation.",
	"proxy candidate stop/state-dir":                               "Absolute, lexically clean path; not /; no symbolic link; owned by current user; parent owner-controlled.",
	"proxy health/port":                                            "Integer 1–65535.",
	"proxy health/timeout":                                         "Positive Go duration, at most 10m; no bare number. Deadline expiry never implies rollback succeeded.",
	"proxy operation status/operation-id":                          "If supplied, exactly 32 lowercase hexadecimal characters.",
	"proxy rescue enter/port":                                      "Integer 1..65535; omission resolves existing bootstrap configuration without creating it.",
	"proxy rescue enter/timeout":                                   "Go duration, 1s <= value <= 5m.",
	"proxy rescue exit/port":                                       "Integer 1..65535; omission resolves existing bootstrap configuration without creating it.",
	"proxy rescue exit/timeout":                                    "Go duration, 1s <= value <= 5m.",
	"proxy rescue status/port":                                     "Integer 1..65535; omission resolves existing bootstrap configuration without creating it.",
	"proxy rescue status/timeout":                                  "Go duration, 1s <= value <= 5m.",
	"proxy serve/port":                                             "No ephemeral port 0; reject a listener owned by another process.",
	"proxy state initialise/state-dir":                             "Required clean absolute non-root path; reject symlink path components and existing foreign/incompatible state.",
	"proxy state initialise/timeout":                               "Positive Go duration, at most 10m; no bare number. Deadline expiry never implies rollback succeeded.",
	"proxy status/state-dir":                                       "When supplied: existing clean absolute non-root directory; no symlink ancestors; never create it.",
	"proxy status/timeout":                                         "Positive Go duration, at most 10m; no bare number. Deadline expiry never implies rollback succeeded.",
	"service install/component":                                    "Exact enum; selection never widens after validation.",
	"service install/timeout":                                      "Positive Go duration, at most 10m; no bare number. Deadline expiry never implies rollback succeeded.",
	"service restart/component":                                    "Exact enum; selection never widens after validation.",
	"service restart/timeout":                                      "Positive Go duration, at most 10m; no bare number. Deadline expiry never implies rollback succeeded.",
	"service start/component":                                      "Exact enum; selection never widens after validation.",
	"service start/timeout":                                        "Positive Go duration, at most 10m; no bare number. Deadline expiry never implies rollback succeeded.",
	"service status/component":                                     "Exact enum; selection never widens after validation.",
	"service status/timeout":                                       "Positive Go duration, at most 10m; no bare number. Deadline expiry never implies rollback succeeded.",
	"service stop/component":                                       "Exact enum; selection never widens after validation.",
	"service stop/timeout":                                         "Positive Go duration, at most 10m; no bare number. Deadline expiry never implies rollback succeeded.",
	"service uninstall/component":                                  "Exact enum; selection never widens after validation.",
	"service uninstall/timeout":                                    "Positive Go duration, at most 10m; no bare number. Deadline expiry never implies rollback succeeded.",
}
