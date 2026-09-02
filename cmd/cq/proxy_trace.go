package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jacobcxdev/cq/internal/proxy"
)

type proxyTraceDependencies struct {
	LoadConfig func() (*proxy.Config, error)
	Now        func() time.Time
	Context    context.Context
}

type proxyTraceOptions struct {
	session string
	traceID string
	since   time.Duration
	tail    int
	follow  bool
	json    bool
	payload bool
}

type proxyTraceRecord struct {
	Time           time.Time       `json:"time"`
	EventType      string          `json:"event_type"`
	TraceID        string          `json:"trace_id"`
	ConnectionID   string          `json:"connection_id"`
	Sequence       uint64          `json:"sequence"`
	SessionKey     string          `json:"session_key"`
	ThreadKey      string          `json:"thread_key"`
	Transport      string          `json:"transport"`
	Phase          string          `json:"phase"`
	Stage          string          `json:"stage"`
	Outcome        string          `json:"outcome"`
	Reason         string          `json:"reason"`
	ErrorClass     string          `json:"error_class"`
	AccountHint    string          `json:"account_hint"`
	Pool           string          `json:"pool"`
	Attempt        int             `json:"attempt"`
	StatusCode     int             `json:"status_code"`
	UpstreamStatus int             `json:"upstream_status"`
	CloseCode      int             `json:"close_code"`
	CloseReason    string          `json:"close_reason"`
	EventName      string          `json:"event_name"`
	Direction      string          `json:"direction"`
	Raw            json.RawMessage `json:"-"`
}

type proxyTraceFilter struct {
	traceID string
	session map[string]struct{}
	since   time.Time
	payload bool
}

type proxyTraceFollower struct {
	file    *os.File
	info    os.FileInfo
	pending []byte
}

type proxyTraceOpenFile struct {
	file *os.File
	info os.FileInfo
	path string
}

func runProxyTrace(args []string, output io.Writer) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return runProxyTraceWith(args, output, proxyTraceDependencies{
		LoadConfig: proxy.LoadExistingConfig,
		Now:        time.Now,
		Context:    ctx,
	})
}

func runProxyTraceWith(args []string, output io.Writer, deps proxyTraceDependencies) error {
	if output == nil || deps.LoadConfig == nil || deps.Now == nil {
		return errors.New("proxy trace dependencies unavailable")
	}
	options, err := parseProxyTraceOptions(args)
	if err != nil {
		return err
	}
	config, err := deps.LoadConfig()
	if err != nil {
		return fmt.Errorf("load proxy config: %w", err)
	}
	if config == nil {
		return errors.New("proxy configuration unavailable")
	}
	path := config.DiagnosticsLog
	if options.payload {
		path = config.PayloadDiagnosticsLog
	}
	if path == "" {
		kind := "diagnostics"
		if options.payload {
			kind = "payload diagnostics"
		}
		return fmt.Errorf("%s log is not configured", kind)
	}
	filter := proxyTraceFilter{traceID: options.traceID, payload: options.payload}
	if options.since > 0 {
		filter.since = deps.Now().Add(-options.since)
	}
	if options.session != "" {
		filter.session = make(map[string]struct{})
		for _, key := range proxy.CodexTraceSessionKeys(options.session) {
			filter.session[key] = struct{}{}
		}
	}
	var records []proxyTraceRecord
	var follower *proxyTraceFollower
	if options.follow {
		records, follower, err = readProxyTraceRecordsForFollow(path, filter)
	} else {
		records, err = readProxyTraceRecords(path, filter)
	}
	if err != nil {
		return err
	}
	if options.tail > 0 && len(records) > options.tail {
		records = records[len(records)-options.tail:]
	}
	for _, record := range records {
		writeProxyTraceRecord(output, record, options.json)
	}
	if !options.follow {
		return nil
	}
	ctx := deps.Context
	if ctx == nil {
		ctx = context.Background()
	}
	return followProxyTrace(ctx, path, filter, output, options.json, follower)
}

func parseProxyTraceOptions(args []string) (proxyTraceOptions, error) {
	options := proxyTraceOptions{tail: 200}
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--session":
			if options.session != "" || index+1 >= len(args) {
				return options, errors.New("usage: cq proxy trace [--session SELECTOR] [--trace ID] [--since DURATION] [--tail N] [--follow] [--json] [--payload]")
			}
			index++
			options.session = args[index]
		case "--trace":
			if options.traceID != "" || index+1 >= len(args) {
				return options, errors.New("proxy trace: invalid --trace")
			}
			index++
			options.traceID = args[index]
		case "--since":
			if options.since != 0 || index+1 >= len(args) {
				return options, errors.New("proxy trace: invalid --since")
			}
			index++
			duration, err := time.ParseDuration(args[index])
			if err != nil || duration <= 0 {
				return options, errors.New("proxy trace: invalid --since")
			}
			options.since = duration
		case "--tail":
			if index+1 >= len(args) {
				return options, errors.New("proxy trace: invalid --tail")
			}
			index++
			value, err := strconv.Atoi(args[index])
			if err != nil || value < 0 {
				return options, errors.New("proxy trace: invalid --tail")
			}
			options.tail = value
		case "--follow":
			options.follow = true
		case "--json":
			options.json = true
		case "--payload":
			options.payload = true
		default:
			return options, fmt.Errorf("proxy trace: unknown argument %q", args[index])
		}
	}
	return options, nil
}

func readProxyTraceRecords(path string, filter proxyTraceFilter) ([]proxyTraceRecord, error) {
	var records []proxyTraceRecord
	for _, candidate := range proxy.DiagnosticsLogPaths(path) {
		file, err := os.Open(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("open diagnostics log: %w", err)
		}
		read, readErr := readProxyTraceStream(file, filter)
		closeErr := file.Close()
		if readErr != nil || closeErr != nil {
			return nil, errors.Join(readErr, closeErr)
		}
		records = append(records, read...)
	}
	return records, nil
}

func readProxyTraceRecordsForFollow(path string, filter proxyTraceFilter) ([]proxyTraceRecord, *proxyTraceFollower, error) {
	return readProxyTraceRecordsForFollowWithHook(path, filter, nil)
}

func readProxyTraceRecordsForFollowWithHook(path string, filter proxyTraceFilter, afterCurrentAttached func()) ([]proxyTraceRecord, *proxyTraceFollower, error) {
	anchor, err := openProxyTraceFile(path)
	if err != nil {
		return nil, nil, err
	}
	if afterCurrentAttached != nil {
		afterCurrentAttached()
	}
	files, err := openProxyTraceFiles(path)
	if err != nil {
		closeErr := closeProxyTraceFile(anchor)
		return nil, nil, errors.Join(err, closeErr)
	}
	if anchor != nil && proxyTraceFileIndex(files, anchor.info) < 0 {
		closeErr := errors.Join(closeProxyTraceFiles(files), closeProxyTraceFile(anchor))
		return nil, nil, errors.Join(errors.New("diagnostics log rotated beyond retained history while checkpointing"), closeErr)
	}
	if err := closeProxyTraceFile(anchor); err != nil {
		_ = closeProxyTraceFiles(files)
		return nil, nil, err
	}
	follower := &proxyTraceFollower{}
	var records []proxyTraceRecord
	for index, opened := range files {
		data, readErr := io.ReadAll(opened.file)
		if readErr != nil {
			_ = closeProxyTraceFiles(files[index:])
			return nil, nil, readErr
		}
		read, pending := parseProxyTraceData(data, filter)
		records = append(records, read...)
		if index != len(files)-1 {
			if len(pending) != 0 {
				_ = closeProxyTraceFiles(files[index:])
				return nil, nil, fmt.Errorf("rotated diagnostics log %q ends with incomplete record", opened.path)
			}
			if err := opened.file.Close(); err != nil {
				_ = closeProxyTraceFiles(files[index+1:])
				return nil, nil, err
			}
			continue
		}
		follower.file = opened.file
		follower.info = opened.info
		follower.pending = pending
	}
	return records, follower, nil
}

func openProxyTraceFiles(path string) ([]proxyTraceOpenFile, error) {
	paths := proxy.DiagnosticsLogPaths(path)
	newestFirst := make([]proxyTraceOpenFile, 0, len(paths))
	for index := len(paths) - 1; index >= 0; index-- {
		opened, err := openProxyTraceFile(paths[index])
		if err != nil {
			_ = closeProxyTraceFiles(newestFirst)
			return nil, err
		}
		if opened == nil {
			continue
		}
		if proxyTraceFileIndex(newestFirst, opened.info) >= 0 {
			if err := opened.file.Close(); err != nil {
				_ = closeProxyTraceFiles(newestFirst)
				return nil, err
			}
			continue
		}
		newestFirst = append(newestFirst, *opened)
	}
	files := make([]proxyTraceOpenFile, len(newestFirst))
	for index := range newestFirst {
		files[len(newestFirst)-1-index] = newestFirst[index]
	}
	return files, nil
}

func openProxyTraceFile(path string) (*proxyTraceOpenFile, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open diagnostics log: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &proxyTraceOpenFile{file: file, info: info, path: path}, nil
}

func proxyTraceFileIndex(files []proxyTraceOpenFile, info os.FileInfo) int {
	for index := range files {
		if os.SameFile(files[index].info, info) {
			return index
		}
	}
	return -1
}

func closeProxyTraceFile(file *proxyTraceOpenFile) error {
	if file == nil || file.file == nil {
		return nil
	}
	return file.file.Close()
}

func closeProxyTraceFiles(files []proxyTraceOpenFile) error {
	var err error
	for index := range files {
		err = errors.Join(err, files[index].file.Close())
	}
	return err
}

func parseProxyTraceData(data []byte, filter proxyTraceFilter) ([]proxyTraceRecord, []byte) {
	lines := bytes.Split(data, []byte{'\n'})
	pending := append([]byte(nil), lines[len(lines)-1]...)
	var records []proxyTraceRecord
	for _, line := range lines[:len(lines)-1] {
		if record, ok := parseProxyTraceRecord(line, filter); ok {
			records = append(records, record)
		}
	}
	return records, pending
}

func readProxyTraceStream(reader io.Reader, filter proxyTraceFilter) ([]proxyTraceRecord, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 128<<20)
	var records []proxyTraceRecord
	for scanner.Scan() {
		record, ok := parseProxyTraceRecord(scanner.Bytes(), filter)
		if ok {
			records = append(records, record)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read diagnostics log: %w", err)
	}
	return records, nil
}

func parseProxyTraceRecord(raw []byte, filter proxyTraceFilter) (proxyTraceRecord, bool) {
	var record proxyTraceRecord
	if json.Unmarshal(raw, &record) != nil || record.TraceID == "" {
		return record, false
	}
	if filter.payload {
		if record.EventType != "codex_payload" {
			return record, false
		}
	} else if record.EventType != "codex_trace" && record.EventType != "" {
		return record, false
	}
	if filter.traceID != "" && record.TraceID != filter.traceID {
		return record, false
	}
	if len(filter.session) != 0 {
		_, sessionMatch := filter.session[record.SessionKey]
		_, threadMatch := filter.session[record.ThreadKey]
		if !sessionMatch && !threadMatch {
			return record, false
		}
	}
	if !filter.since.IsZero() && record.Time.Before(filter.since) {
		return record, false
	}
	record.Raw = append(json.RawMessage(nil), raw...)
	return record, true
}

func writeProxyTraceRecord(output io.Writer, record proxyTraceRecord, jsonOutput bool) {
	if jsonOutput {
		_, _ = output.Write(append(record.Raw, '\n'))
		return
	}
	phase := record.Phase
	if phase == "" {
		phase = "route_summary"
	}
	status := record.StatusCode
	if record.UpstreamStatus != 0 {
		status = record.UpstreamStatus
	}
	parts := []string{
		record.Time.Format(time.RFC3339Nano), record.TraceID, fmt.Sprintf("#%d", record.Sequence), record.Transport, phase,
	}
	for _, value := range []string{record.Outcome, record.Stage, record.Direction, record.EventName, record.AccountHint} {
		if value != "" {
			parts = append(parts, value)
		}
	}
	if record.Attempt != 0 {
		parts = append(parts, fmt.Sprintf("attempt=%d", record.Attempt))
	}
	if status != 0 {
		parts = append(parts, fmt.Sprintf("status=%d", status))
	}
	if record.Pool != "" {
		parts = append(parts, "pool="+record.Pool)
	}
	if record.CloseCode != 0 {
		parts = append(parts, fmt.Sprintf("close=%d", record.CloseCode))
	}
	if record.CloseReason != "" {
		parts = append(parts, "close_reason="+record.CloseReason)
	}
	if record.ErrorClass != "" {
		parts = append(parts, "error_class="+record.ErrorClass)
	}
	if record.Reason != "" {
		parts = append(parts, "reason="+record.Reason)
	}
	_, _ = fmt.Fprintln(output, strings.Join(parts, " "))
}

func followProxyTrace(ctx context.Context, path string, filter proxyTraceFilter, output io.Writer, jsonOutput bool, follower *proxyTraceFollower) error {
	if follower == nil {
		follower = &proxyTraceFollower{}
	}
	defer follower.Close()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := follower.ReadAvailable(path, filter, output, jsonOutput); err != nil {
				return err
			}
		}
	}
}

func (follower *proxyTraceFollower) ReadAvailable(path string, filter proxyTraceFilter, output io.Writer, jsonOutput bool) error {
	return follower.readAvailableWithHook(path, filter, output, jsonOutput, nil)
}

func (follower *proxyTraceFollower) readAvailableWithHook(path string, filter proxyTraceFilter, output io.Writer, jsonOutput bool, afterBridgeAttached func()) error {
	if follower.file == nil {
		records, attached, err := readProxyTraceRecordsForFollow(path, filter)
		if err != nil {
			return err
		}
		for _, record := range records {
			writeProxyTraceRecord(output, record, jsonOutput)
		}
		*follower = *attached
		return nil
	}
	currentInfo, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if os.SameFile(follower.info, currentInfo) {
		position, seekErr := follower.file.Seek(0, io.SeekCurrent)
		if seekErr != nil {
			return seekErr
		}
		if currentInfo.Size() < position {
			if _, seekErr = follower.file.Seek(0, io.SeekStart); seekErr != nil {
				return seekErr
			}
			follower.pending = nil
		}
		return follower.readOpen(filter, output, jsonOutput)
	}
	files, err := openProxyTraceFiles(path)
	if err != nil {
		return err
	}
	previousIndex := proxyTraceFileIndex(files, follower.info)
	if previousIndex < 0 {
		_ = closeProxyTraceFiles(files)
		return errors.New("diagnostics log rotated beyond retained history while following")
	}
	if afterBridgeAttached != nil {
		afterBridgeAttached()
	}
	if err := follower.readOpen(filter, output, jsonOutput); err != nil {
		_ = closeProxyTraceFiles(files)
		return err
	}
	if len(follower.pending) != 0 {
		_ = closeProxyTraceFiles(files)
		return errors.New("diagnostics log rotated with incomplete record")
	}
	if err := closeProxyTraceFiles(files[:previousIndex+1]); err != nil {
		_ = closeProxyTraceFiles(files[previousIndex+1:])
		return err
	}
	successors := files[previousIndex+1:]
	if len(successors) == 0 {
		return nil
	}
	if err := follower.Close(); err != nil {
		_ = closeProxyTraceFiles(successors)
		return err
	}
	for index, successor := range successors {
		data, readErr := io.ReadAll(successor.file)
		if readErr != nil {
			_ = closeProxyTraceFiles(successors[index:])
			return readErr
		}
		pending := writeProxyTraceData(data, nil, filter, output, jsonOutput)
		if index != len(successors)-1 {
			if len(pending) != 0 {
				_ = closeProxyTraceFiles(successors[index:])
				return fmt.Errorf("rotated diagnostics log %q ends with incomplete record", successor.path)
			}
			if err := successor.file.Close(); err != nil {
				_ = closeProxyTraceFiles(successors[index+1:])
				return err
			}
			continue
		}
		follower.file = successor.file
		follower.info = successor.info
		follower.pending = pending
	}
	return nil
}
func (follower *proxyTraceFollower) readOpen(filter proxyTraceFilter, output io.Writer, jsonOutput bool) error {
	data, err := io.ReadAll(follower.file)
	if err != nil {
		return err
	}
	follower.pending = writeProxyTraceData(data, follower.pending, filter, output, jsonOutput)
	return nil
}

func (follower *proxyTraceFollower) Close() error {
	if follower == nil || follower.file == nil {
		return nil
	}
	err := follower.file.Close()
	follower.file = nil
	follower.info = nil
	follower.pending = nil
	return err
}

func writeProxyTraceData(data, pending []byte, filter proxyTraceFilter, output io.Writer, jsonOutput bool) []byte {
	data = append(pending, data...)
	lines := bytes.Split(data, []byte{'\n'})
	pending = append(pending[:0], lines[len(lines)-1]...)
	for _, line := range lines[:len(lines)-1] {
		if record, ok := parseProxyTraceRecord(line, filter); ok {
			writeProxyTraceRecord(output, record, jsonOutput)
		}
	}
	return pending
}
