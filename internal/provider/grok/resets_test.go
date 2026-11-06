package grok

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func resetVarint(n uint64) []byte {
	var b [10]byte
	l := binary.PutUvarint(b[:], n)
	return append([]byte(nil), b[:l]...)
}
func resetField(n uint64, b []byte) []byte {
	return append(append(resetVarint(n<<3|2), resetVarint(uint64(len(b)))...), b...)
}
func resetTimestamp(t time.Time) []byte { return append([]byte{8}, resetVarint(uint64(t.Unix()))...) }
func resetFrame(flag byte, b []byte) []byte {
	r := []byte{flag, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(r[1:], uint32(len(b)))
	return append(r, b...)
}
func resetFixture(now time.Time) []byte {
	token := append(resetField(10, []byte("private-redemption-id")), resetField(20, resetTimestamp(now.Add(-time.Hour)))...)
	token = append(token, resetField(30, resetTimestamp(now.Add(time.Hour)))...)
	b := resetFrame(0, resetField(10, token))
	return append(b, resetFrame(0x80, []byte("grpc-status: 0\r\n"))...)
}

func TestParseResetInventoryRejectsMalformedAndExpired(t *testing.T) {
	now := time.Unix(1800000000, 0)
	rewards, err := parseResetInventory(resetFixture(now), now)
	if err != nil || len(rewards) != 1 || rewards[0].ID != "private-redemption-id" || !rewards[0].ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("count=%d err=%v", len(rewards), err)
	}
	for name, data := range map[string][]byte{"truncated": {0, 0, 0, 0, 9, 1}, "status": append(resetFixture(now), resetFrame(0x80, []byte("grpc-status: 7\r\n"))...), "overflow": resetFrame(0, bytes.Repeat([]byte{255}, 11)), "unknown": resetFrame(0, resetField(1, []byte("anything")))} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseResetInventory(data, now); err == nil {
				t.Fatal("malformed inventory accepted")
			}
		})
	}
	rewards, err = parseResetInventory(resetFixture(now), now.Add(2*time.Hour))
	if err != nil || len(rewards) != 0 {
		t.Fatal("expired reward retained")
	}
}

func TestParseResetInventoryAcceptsCanonicalEmptyMessage(t *testing.T) {
	rewards, err := parseResetInventory([]byte{0, 0, 0, 0, 0}, time.Now())
	if err != nil || len(rewards) != 0 {
		t.Fatalf("empty inventory failed: %v", err)
	}
}

type resetTestAuthority struct {
	codexBarOwned
	do func(*http.Request) (*http.Response, error)
}

func (a resetTestAuthority) DoResetRead(_ context.Context, _ AccountID, r *http.Request) (*http.Response, error) {
	return a.do(r)
}

func TestListResetsUsesScopedReadAndRedactsRewardIDs(t *testing.T) {
	now := time.Unix(1800000000, 0)
	a := resetTestAuthority{do: func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.URL.String() != resetInventoryURL || !bytes.Equal(body, []byte{0, 0, 0, 0, 0}) {
			t.Fatal("incorrect inventory read")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(resetFixture(now)))}, nil
	}}
	rewards, err := ListResets(context.Background(), a, "account", now)
	if err != nil || len(rewards) != 1 {
		t.Fatalf("count=%d err=%v", len(rewards), err)
	}
	public, _ := json.Marshal(rewards)
	if strings.Contains(string(public), "private-redemption-id") {
		t.Fatal("redemption identifier leaked")
	}
}
