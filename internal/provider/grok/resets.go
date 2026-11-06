package grok

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jacobcxdev/cq/internal/httputil"
)

const resetInventoryURL = "https://grok.com/prod_mc_billing.ConsumerUiSvc/GetRemainingResets"

// ResetReward is an expiring included-quota reset. Redemption identifiers remain
// private; listing rewards never grants or consumes quota.
type ResetReward struct {
	ID        string    `json:"-"`
	GrantedAt time.Time `json:"granted_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type resetReadAuthority interface {
	DoResetRead(context.Context, AccountID, *http.Request) (*http.Response, error)
}

// ListResets uses a separately scoped read capability; the ordinary subscription
// authority must not send credentials to arbitrary grok.com endpoints.
func ListResets(ctx context.Context, authority Authority, id AccountID, now time.Time) ([]ResetReward, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	scoped, ok := authority.(resetReadAuthority)
	if !ok {
		return nil, errors.New("Grok reset inventory authority unavailable")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, resetInventoryURL, bytes.NewReader([]byte{0, 0, 0, 0, 0}))
	if err != nil {
		return nil, errors.New("create Grok reset inventory request")
	}
	request.Header.Set("Content-Type", "application/grpc-web+proto")
	request.Header.Set("X-Grpc-Web", "1")
	request.Header.Set("X-User-Agent", "connect-es/2.1.1")
	request.Header.Set("Origin", "https://grok.com")
	request.Header.Set("Referer", "https://grok.com/?_s=usage")
	request.Header.Set("Accept", "*/*")
	response, err := scoped.DoResetRead(ctx, id, request)
	if err != nil {
		return nil, err
	}
	if response == nil || response.Body == nil {
		return nil, errors.New("invalid Grok reset inventory response")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, subscriptionHTTPError{status: response.StatusCode}
	}
	if status := response.Header.Get("Grpc-Status"); status != "" && status != "0" {
		return nil, errors.New("Grok reset inventory rejected")
	}
	body, err := httputil.ReadBody(response.Body)
	if err != nil {
		return nil, errors.New("read Grok reset inventory")
	}
	return parseResetInventory(body, now)
}

func parseResetInventory(data []byte, now time.Time) ([]ResetReward, error) {
	invalid := errors.New("invalid Grok reset inventory")
	if len(data) == 0 || len(data) > httputil.MaxResponseBody {
		return nil, invalid
	}
	var messages [][]byte
	for len(data) > 0 {
		if len(data) < 5 {
			return nil, invalid
		}
		flag := data[0]
		size := uint64(binary.BigEndian.Uint32(data[1:5]))
		data = data[5:]
		if size > uint64(len(data)) {
			return nil, invalid
		}
		payload := data[:int(size)]
		data = data[int(size):]
		switch flag {
		case 0:
			messages = append(messages, payload)
		case 0x80:
			for _, line := range strings.Split(string(payload), "\r\n") {
				name, value, ok := strings.Cut(line, ":")
				if ok && strings.EqualFold(strings.TrimSpace(name), "grpc-status") {
					n, e := strconv.Atoi(strings.TrimSpace(value))
					if e != nil || n != 0 {
						return nil, errors.New("Grok reset inventory rejected")
					}
				}
			}
		default:
			return nil, invalid
		}
	}
	var rewards []ResetReward
	seen := make(map[string]bool)
	found := false
	for _, message := range messages {
		if len(message) == 0 {
			found = true
			continue
		}
		fields, err := resetFields(message)
		if err != nil {
			return nil, invalid
		}
		for _, field := range fields {
			if field.number != 10 {
				continue
			}
			if field.wire != 2 {
				return nil, invalid
			}
			found = true
			reward, err := parseResetReward(field.bytes)
			if err != nil || seen[reward.ID] {
				return nil, invalid
			}
			seen[reward.ID] = true
			if reward.ExpiresAt.After(now) {
				rewards = append(rewards, reward)
			}
		}
	}
	// An empty data message is the canonical zero inventory. A nonempty
	// response without the schema's token records is unknown, not zero.
	if !found {
		return nil, invalid
	}
	sort.Slice(rewards, func(i, j int) bool { return rewards[i].ExpiresAt.Before(rewards[j].ExpiresAt) })
	return rewards, nil
}

type resetFieldValue struct {
	number  uint64
	wire    byte
	bytes   []byte
	integer uint64
}

func resetFields(data []byte) ([]resetFieldValue, error) {
	invalid := errors.New("invalid Grok reset protobuf")
	var fields []resetFieldValue
	for len(data) > 0 {
		tag, n := binary.Uvarint(data)
		if n <= 0 || tag>>3 == 0 || tag>>3 > 536870911 {
			return nil, invalid
		}
		data = data[n:]
		field := resetFieldValue{number: tag >> 3, wire: byte(tag & 7)}
		switch field.wire {
		case 0:
			value, n := binary.Uvarint(data)
			if n <= 0 {
				return nil, invalid
			}
			field.integer = value
			data = data[n:]
		case 1:
			if len(data) < 8 {
				return nil, invalid
			}
			data = data[8:]
		case 2:
			size, n := binary.Uvarint(data)
			if n <= 0 {
				return nil, invalid
			}
			data = data[n:]
			if size > uint64(len(data)) {
				return nil, invalid
			}
			field.bytes = data[:int(size)]
			data = data[int(size):]
		case 5:
			if len(data) < 4 {
				return nil, invalid
			}
			data = data[4:]
		default:
			return nil, invalid
		}
		fields = append(fields, field)
	}
	return fields, nil
}

func parseResetReward(data []byte) (ResetReward, error) {
	invalid := errors.New("invalid Grok reset reward")
	fields, err := resetFields(data)
	if err != nil {
		return ResetReward{}, invalid
	}
	var reward ResetReward
	seen := map[uint64]bool{}
	for _, field := range fields {
		if field.number != 10 && field.number != 20 && field.number != 30 {
			continue
		}
		if field.wire != 2 || seen[field.number] {
			return reward, invalid
		}
		seen[field.number] = true
		switch field.number {
		case 10:
			if len(field.bytes) == 0 || len(field.bytes) > 4096 || !utf8.Valid(field.bytes) {
				return reward, invalid
			}
			reward.ID = string(field.bytes)
		case 20:
			reward.GrantedAt, err = resetTime(field.bytes)
		case 30:
			reward.ExpiresAt, err = resetTime(field.bytes)
		}
		if err != nil {
			return reward, invalid
		}
	}
	if reward.ID == "" || reward.ExpiresAt.IsZero() {
		return reward, invalid
	}
	return reward, nil
}

func resetTime(data []byte) (time.Time, error) {
	fields, err := resetFields(data)
	if err != nil {
		return time.Time{}, err
	}
	var seconds, nanos uint64
	seen := map[uint64]bool{}
	for _, field := range fields {
		if field.number != 1 && field.number != 2 {
			continue
		}
		if field.wire != 0 || seen[field.number] {
			return time.Time{}, errors.New("invalid Grok reset timestamp")
		}
		seen[field.number] = true
		if field.number == 1 {
			seconds = field.integer
		} else {
			nanos = field.integer
		}
	}
	if !seen[1] || seconds == 0 || seconds > 253402300799 || nanos > 999999999 {
		return time.Time{}, errors.New("invalid Grok reset timestamp")
	}
	return time.Unix(int64(seconds), int64(nanos)).UTC(), nil
}
