package harness

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

// RetryRequest authorises one retry of one failed full-mode application. It is
// private controller intent, never part of shared policy or portable profiles.
type RetryRequest struct {
	ChatID      string    `json:"chat_id"`
	Nonce       string    `json:"nonce"`
	Fingerprint string    `json:"fingerprint"`
	Model       string    `json:"model"`
	Cwd         string    `json:"cwd"`
	FailedAt    time.Time `json:"failed_at"`
}

func (request RetryRequest) Matches(receipt Receipt) bool {
	return request.ChatID == receipt.ChatID && receipt.Mode == "full" && receipt.RetrySupported && receipt.State == "failed" && request.Fingerprint == receipt.Fingerprint && request.Model == receipt.Model && request.Cwd == receipt.Cwd && request.FailedAt.Equal(receipt.UpdatedAt)
}

// LoadRetry reads only this chat's bounded request without acquiring its native
// lifecycle lease. Missing requests create no directories or files.
func (receipts Receipts) LoadRetry(chatID string) (RetryRequest, error) {
	path, err := receipts.path(chatID, ".retry.json")
	if err != nil {
		return RetryRequest{}, err
	}
	data, err := fsutil.ReadSecureFile(receipts.FS, path, maxPolicyBytes)
	if err != nil {
		return RetryRequest{}, err
	}
	var request RetryRequest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return RetryRequest{}, fmt.Errorf("invalid harness retry request")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return RetryRequest{}, fmt.Errorf("invalid harness retry trailing data")
	}
	if err := validateRetryRequest(chatID, request); err != nil {
		return RetryRequest{}, err
	}
	return request, nil
}

// RequestRetry coalesces requests for the same failed receipt under a separate
// intent lock. The running owner retains its native lifecycle lease throughout.
func (receipts Receipts) RequestRetry(chatID string, request RetryRequest) (RetryRequest, error) {
	request.ChatID = chatID
	request.Nonce = "validation"
	if err := validateRetryRequest(chatID, request); err != nil {
		return RetryRequest{}, err
	}
	receipt, err := receipts.Load(chatID)
	if err != nil {
		return RetryRequest{}, err
	}
	if !request.Matches(receipt) {
		return RetryRequest{}, fmt.Errorf("harness retry no longer matches a supported failed receipt")
	}
	path, err := receipts.path(chatID, ".retry.json")
	if err != nil {
		return RetryRequest{}, err
	}
	lockPath, err := receipts.path(chatID, ".retry.lock")
	if err != nil {
		return RetryRequest{}, err
	}
	lock, err := fsutil.AcquireExclusiveLock(receipts.FS, lockPath)
	if err != nil {
		return RetryRequest{}, err
	}
	defer lock.Close()
	receipt, err = receipts.Load(chatID)
	if err != nil {
		return RetryRequest{}, err
	}
	if !request.Matches(receipt) {
		return RetryRequest{}, fmt.Errorf("harness retry receipt changed before request")
	}
	previous, err := receipts.LoadRetry(chatID)
	if err == nil && previous.Matches(receipt) {
		return previous, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return RetryRequest{}, err
	}
	request.Nonce = rand.Text()
	data, err := json.MarshalIndent(request, "", "  ")
	if err != nil {
		return RetryRequest{}, err
	}
	if err := fsutil.SecureAtomicWrite(receipts.FS, path, append(data, '\n')); err != nil {
		return RetryRequest{}, err
	}
	return request, nil
}

func validateRetryRequest(chat string, request RetryRequest) error {
	if !canonicalChatID(chat) || request.ChatID != chat || request.Nonce == "" || len(request.Nonce) > 128 || request.FailedAt.IsZero() || request.Model == "" || len(request.Model) > 1024 || strings.ContainsRune(request.Model, '\x00') || !filepath.IsAbs(request.Cwd) || filepath.Clean(request.Cwd) != request.Cwd || strings.ContainsRune(request.Cwd, '\x00') {
		return fmt.Errorf("invalid harness retry request")
	}
	for _, character := range request.Nonce {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_') {
			return fmt.Errorf("invalid harness retry nonce")
		}
	}
	if decoded, err := hex.DecodeString(request.Fingerprint); err != nil || len(decoded) != 32 || strings.ToLower(request.Fingerprint) != request.Fingerprint {
		return fmt.Errorf("invalid harness retry fingerprint")
	}
	return nil
}
