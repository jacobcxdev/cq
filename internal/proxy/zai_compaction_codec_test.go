package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

const zaiCodecTestToken = "private-test-proxy-token"
const zaiCodecTestSummary = "Private summary: completed fixture scan; next read β.txt. Preserve findings and unfinished work."

func zaiCodecItem(t *testing.T, summary string) json.RawMessage {
	t.Helper()
	item, err := sealZAICompaction(zaiCodecTestToken, summary)
	if err != nil {
		t.Fatal("seal fixture summary")
	}
	return item
}

func zaiCodecInput(item json.RawMessage) []byte {
	return append(append([]byte(`{"model":"glm-5.3","precise":9007199254740993,"input":[`), item...), []byte(`]}`)...)
}

func TestZAICompactionCodecRoundTripAndIndependentAccounts(t *testing.T) {
	item := zaiCodecItem(t, zaiCodecTestSummary)
	var sealed struct {
		Type, ID  string
		Encrypted string `json:"encrypted_content"`
	}
	if json.Unmarshal(item, &sealed) != nil || sealed.Type != "compaction" || !strings.HasPrefix(sealed.ID, "cq_zai_compaction_") || !strings.HasPrefix(sealed.Encrypted, zaiCompactionPrefix) {
		t.Fatal("invalid sealed fixture shape")
	}
	if bytes.Contains(item, []byte(zaiCodecTestSummary)) || bytes.Contains(item, []byte(zaiCodecTestToken)) {
		t.Fatal("sealed item exposed private data")
	}
	for _, account := range []string{"first-account", "second-account"} {
		body := []byte(`{"model":"glm-5.3","account":"` + account + `","precise":9007199254740993,"input":[` + string(item) + `]}`)
		original := append([]byte(nil), body...)
		updated, changed, err := normaliseCQCompactionInput(body, zaiCodecTestToken)
		if err != nil || !changed {
			t.Fatal("restore fixture summary")
		}
		if !bytes.Equal(body, original) || !bytes.Contains(updated, []byte(`9007199254740993`)) || !bytes.Contains(updated, []byte(account)) {
			t.Fatal("request data changed unexpectedly")
		}
		var decoded struct {
			Input []struct {
				Type, Role string
				Content    []struct{ Type, Text string }
			}
		}
		if json.Unmarshal(updated, &decoded) != nil || len(decoded.Input) != 1 || decoded.Input[0].Type != "message" || decoded.Input[0].Role != "assistant" || len(decoded.Input[0].Content) != 1 || decoded.Input[0].Content[0].Type != "output_text" || decoded.Input[0].Content[0].Text != zaiCodecTestSummary {
			t.Fatal("summary was not restored as assistant text")
		}
	}
	second := zaiCodecItem(t, zaiCodecTestSummary)
	if bytes.Equal(item, second) {
		t.Fatal("reused compaction nonce")
	}
}

func TestZAICompactionCodecUnchangedRequests(t *testing.T) {
	for _, body := range []string{
		" { \"input\" : [ {\"type\":\"message\",\"role\":\"user\",\"content\":\"ordinary\"} ], \"counter\":9007199254740993 } ",
		`{"input":[{"type":"compaction","encrypted_content":"foreign-opaque-state"}]}`,
		`{"input":[{"type":"context_compaction","encrypted_content":"foreign-opaque-state"}]}`,
		`{"input":"cq_compaction_v1: literal user text"}`,
		`{"input":[{"type":"function_call_output","output":{"input":[{"type":"compaction","encrypted_content":"cq_compaction_v1:not-top-level"}]}}]}`,
		`{"input":[{"type":"message","role":"assistant","encrypted_content":"cq_compaction_v1:ordinary-message"}]}`,
		`{"input":null}`,
		`{"other":1e100}`,
		`{"input":[],"input":[{"type":"message","role":"user","content":"ordinary"}]}`,
		`{"input":[],"input":[{"type":"message","content":"cq_compaction_v1: is literal text"}]}`,
		`[1,2]`,
		`null`,
	} {
		updated, changed, err := normaliseCQCompactionInput([]byte(body), "")
		if err != nil || changed || string(updated) != body {
			t.Fatal("unowned request did not retain exact bytes")
		}
	}
}

func TestZAICompactionCodecTamperingAndWrongToken(t *testing.T) {
	item := zaiCodecItem(t, zaiCodecTestSummary)
	var sealed map[string]json.RawMessage
	if json.Unmarshal(item, &sealed) != nil {
		t.Fatal("decode fixture")
	}
	var encoded string
	if json.Unmarshal(sealed["encrypted_content"], &encoded) != nil {
		t.Fatal("decode fixture state")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(encoded, zaiCompactionPrefix))
	if err != nil {
		t.Fatal("decode fixture cipher")
	}
	for _, test := range []struct{ name, token, encoded string }{
		{"wrong token", "different-private-token", encoded},
		{"missing token", "", encoded},
		{"invalid base64", zaiCodecTestToken, zaiCompactionPrefix + "!"},
		{"noncanonical base64 newline", zaiCodecTestToken, encoded + "\n"},
		{"padded base64", zaiCodecTestToken, encoded + "="},
		{"truncated cipher", zaiCodecTestToken, zaiCompactionPrefix + base64.RawURLEncoding.EncodeToString([]byte("short"))},
		{"oversized cipher", zaiCodecTestToken, zaiCompactionPrefix + strings.Repeat("A", 180000)},
	} {
		t.Run(test.name, func(t *testing.T) {
			bad := []byte(`{"type":"compaction","encrypted_content":` + string(zaiCodecJSONString(t, test.encoded)) + `}`)
			zaiCodecAssertRestoreError(t, zaiCodecInput(bad), test.token)
		})
	}
	for _, index := range []int{0, len(payload) - 1} {
		changed := append([]byte(nil), payload...)
		changed[index] ^= 1
		bad := []byte(`{"type":"compaction","encrypted_content":` + string(zaiCodecJSONString(t, zaiCompactionPrefix+base64.RawURLEncoding.EncodeToString(changed))) + `}`)
		zaiCodecAssertRestoreError(t, zaiCodecInput(bad), zaiCodecTestToken)
	}
}

func zaiCodecJSONString(t *testing.T, value string) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal("encode fixture string")
	}
	return raw
}

func zaiCodecAssertRestoreError(t *testing.T, body []byte, token string) {
	t.Helper()
	updated, changed, err := normaliseCQCompactionInput(body, token)
	if err == nil || changed || updated != nil {
		t.Fatal("invalid state was not rejected atomically")
	}
	for _, secret := range []string{zaiCodecTestToken, zaiCodecTestSummary, "different-private-token"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("private data leaked through restore error")
		}
	}
}

func TestZAICompactionCodecMalformedAuthority(t *testing.T) {
	item := string(zaiCodecItem(t, zaiCodecTestSummary))
	var envelope map[string]json.RawMessage
	if json.Unmarshal([]byte(item), &envelope) != nil {
		t.Fatal("decode fixture")
	}
	state := string(envelope["encrypted_content"])
	for _, body := range []string{
		`{"input":[],"input":[` + item + `]}`,
		`{"input":[` + item + `],"input":[]}`,
		`{"INPUT":[],"input":[` + item + `]}`,
		`{"input":[{"type":"message","type":"compaction","encrypted_content":` + state + `}]}`,
		`{"input":[{"type":"compaction","encrypted_content":"foreign","encrypted_content":` + state + `}]}`,
		`{"input":[{"type":"compaction","encrypted_content":` + state + `,"encrypted_content":"foreign"}]}`,
		`{"input":[{"type":"compaction","id":"one","ID":"two","encrypted_content":` + state + `}]}`,
		`{"input":[{"type":true,"encrypted_content":` + state + `}]}`,
		`{"input":[{"type":null,"encrypted_content":` + state + `}]}`,
		`{"input":[{"encrypted_content":` + state + `}]}`,
		`{"input":[{"type":"compaction","id":42,"encrypted_content":` + state + `}]}`,
		`{"input":[` + item + `]} trailing`,
	} {
		zaiCodecAssertRestoreError(t, []byte(body), zaiCodecTestToken)
	}
}

func TestZAICompactionCodecLargeOrdinaryRequestsRetainBytes(t *testing.T) {
	for _, suffix := range []string{"", zaiCompactionPrefix + "literal text"} {
		body := []byte(`{"model":"gpt-test","input":[{"role":"user","content":"` + strings.Repeat("x", maxRequestBody) + suffix + `"}]}`)
		updated, changed, err := normaliseCQCompactionInput(body, "")
		if err != nil || changed || !bytes.Equal(updated, body) {
			t.Fatal("ordinary request inherited CQ compaction size limit")
		}
	}
}

func TestZAICompactionCodecLargeOwnedRequestsUseProviderBudget(t *testing.T) {
	item := zaiCodecItem(t, zaiCodecTestSummary)
	attachment := []byte(`{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,` + strings.Repeat("A", maxRequestBody+1) + `"}],"precise":9007199254740993}`)
	body := []byte(`{"model":"gpt-test","input":[` + string(item) + `,` + string(attachment) + `]}`)
	updated, changed, err := normaliseCQCompactionInput(body, zaiCodecTestToken)
	if err != nil || !changed {
		t.Fatal("CQ restoration applied an unrelated provider request limit")
	}
	var request struct{ Input []json.RawMessage }
	if json.Unmarshal(updated, &request) != nil || len(request.Input) != 2 {
		t.Fatal("large request input changed unexpectedly")
	}
	if !bytes.Contains(request.Input[0], []byte("Private summary:")) || !bytes.Equal(request.Input[1], attachment) {
		t.Fatal("large attachment bytes or numeric precision changed during restoration")
	}
}

func TestZAICompactionCodecEscapedMarkerRestoresAndFailsClosed(t *testing.T) {
	item := zaiCodecItem(t, zaiCodecTestSummary)
	item = bytes.Replace(item, []byte(zaiCompactionPrefix), []byte(`\u0063q_compaction_v1\u003a`), 1)
	body := zaiCodecInput(item)
	if _, changed, err := normaliseCQCompactionInput(body, zaiCodecTestToken); err != nil || !changed {
		t.Fatal("escaped CQ carrier was not restored")
	}
	zaiCodecAssertRestoreError(t, body, "")
}

func TestZAICompactionCodecSummaryBounds(t *testing.T) {
	for _, summary := range []string{"", " \n\t", string([]byte{0xff}), strings.Repeat("x", zaiCompactionMaxSummaryBytes+1)} {
		if item, err := sealZAICompaction(zaiCodecTestToken, summary); err == nil || item != nil {
			t.Fatal("invalid summary accepted")
		}
	}
	if item, err := sealZAICompaction("", zaiCodecTestSummary); err == nil || item != nil {
		t.Fatal("missing sealing key accepted")
	}
	boundary := strings.Repeat("x", zaiCompactionMaxSummaryBytes)
	item := zaiCodecItem(t, boundary)
	if _, changed, err := normaliseCQCompactionInput(zaiCodecInput(item), zaiCodecTestToken); err != nil || !changed {
		t.Fatal("boundary summary failed round trip")
	}
}

func TestZAICompactionCodecRejectsInvalidDecryptedSummaryAndAAD(t *testing.T) {
	aead, err := zaiCompactionAEAD(zaiCodecTestToken)
	if err != nil {
		t.Fatal("create fixture cipher")
	}
	for _, test := range []struct {
		plain string
		aad   []byte
	}{
		{"", []byte(zaiCompactionPrefix)},
		{" \n", []byte(zaiCompactionPrefix)},
		{string([]byte{0xff}), []byte(zaiCompactionPrefix)},
		{zaiCodecTestSummary, nil},
	} {
		nonce := make([]byte, aead.NonceSize())
		cipher := aead.Seal(append([]byte(nil), nonce...), nonce, []byte(test.plain), test.aad)
		item := []byte(`{"type":"compaction","encrypted_content":` + string(zaiCodecJSONString(t, zaiCompactionPrefix+base64.RawURLEncoding.EncodeToString(cipher))) + `}`)
		zaiCodecAssertRestoreError(t, zaiCodecInput(item), zaiCodecTestToken)
	}
}

func TestZAICompactionCodecMultipleItemsKeepOrderAndPrecision(t *testing.T) {
	first := zaiCodecItem(t, "First summary")
	second := zaiCodecItem(t, "Second summary")
	body := []byte(`{"input":[` + string(first) + `,{"type":"function_call_output","call_id":"call-1","output":{"counter":9007199254740993}},` + string(second) + `,{"type":"compaction","encrypted_content":"foreign-state"}]}`)
	updated, changed, err := normaliseCQCompactionInput(body, zaiCodecTestToken)
	if err != nil || !changed {
		t.Fatal("restore multiple fixture items")
	}
	var request struct{ Input []json.RawMessage }
	if json.Unmarshal(updated, &request) != nil || len(request.Input) != 4 {
		t.Fatal("fixture item order/count changed")
	}
	if !bytes.Contains(request.Input[0], []byte("First summary")) || !bytes.Contains(request.Input[1], []byte("9007199254740993")) || !bytes.Contains(request.Input[2], []byte("Second summary")) || !bytes.Contains(request.Input[3], []byte("foreign-state")) {
		t.Fatal("fixture item order or numeric precision changed")
	}
}
