package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const liveNormalStatusContext = "cq/live-normal-routing"

type combinedStatus struct {
	SHA      string `json:"sha"`
	Statuses []struct {
		Context string `json:"context"`
		State   string `json:"state"`
	} `json:"statuses"`
}

func main() {
	if len(os.Args) != 3 || os.Args[1] != "--sha" {
		fmt.Fprintln(os.Stderr, "releasestatus: usage: releasestatus --sha COMMIT")
		os.Exit(2)
	}
	if err := requireLiveNormalStatus(os.Stdin, os.Args[2]); err != nil {
		fmt.Fprintf(os.Stderr, "releasestatus: %v\n", err)
		os.Exit(1)
	}
}

func requireLiveNormalStatus(reader io.Reader, wantSHA string) error {
	if len(wantSHA) != 40 || strings.Trim(wantSHA, "0123456789abcdef") != "" {
		return errors.New("release commit must be 40 lower-case hexadecimal characters")
	}
	body, err := io.ReadAll(io.LimitReader(reader, (1<<20)+1))
	if err != nil {
		return fmt.Errorf("read combined commit status: %w", err)
	}
	if len(body) > 1<<20 {
		return errors.New("combined commit status exceeds limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	var status combinedStatus
	if err := decoder.Decode(&status); err != nil {
		return fmt.Errorf("decode combined commit status: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("combined commit status has trailing JSON")
	}
	if status.SHA != wantSHA {
		return errors.New("combined status belongs to another commit")
	}
	for _, candidate := range status.Statuses {
		if candidate.Context != liveNormalStatusContext {
			continue
		}
		if candidate.State != "success" {
			return fmt.Errorf("%s status is %s", liveNormalStatusContext, candidate.State)
		}
		return nil
	}
	return fmt.Errorf("%s status is missing", liveNormalStatusContext)
}
