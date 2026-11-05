package zcode

import (
	_ "embed"
	"encoding/json"
	"errors"
	"strings"
)

const startProductIdentity = "You are ZCode, an interactive agent"

// The public identity text is from ZCode 3.17.2, revision a9fe401eede6e6e5e01be9048679c206fe6f02d9:
// apps/zcode-cli/packages/core/src/context/sections/identity.ts.
// Copyright 2026 Z.AI Co., Ltd. Licensed under Apache-2.0; see third_party/zcode/LICENSE.
// This is the generic identity only, without ZCode Desktop capabilities or paths.
//
//go:embed start_identity.txt
var startAgentIdentity string

// Start accepts the source identity followed by one caller-owned instruction
// block. Keep the caller's text and ordering while avoiding a separate block
// for every Codex instruction item. Tool definitions and history stay intact.
func gatewayStartSystem(body []byte) ([]byte, error) {
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	var caller []struct {
		Type string
		Text *string
	}
	if len(request["system"]) != 0 {
		if err := json.Unmarshal(request["system"], &caller); err != nil {
			return nil, err
		}
	}
	blocks := []map[string]string{
		{"type": "text", "text": startProductIdentity},
		{"type": "text", "text": startAgentIdentity},
	}
	if len(caller) != 0 {
		parts := make([]string, len(caller))
		for i, block := range caller {
			if block.Type != "text" || block.Text == nil {
				return nil, errors.New("unsupported Start system content")
			}
			parts[i] = *block.Text
		}
		blocks = append(blocks, map[string]string{"type": "text", "text": strings.Join(parts, "\n\n")})
	}
	request["system"], _ = json.Marshal(blocks)
	return json.Marshal(request)
}
