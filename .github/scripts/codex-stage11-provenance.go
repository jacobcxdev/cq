package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
)

type reviewedManifest struct {
	Revision         string `json:"revision"`
	TranscriptSHA256 string `json:"transcript_sha256"`
	SmokeSHA256      string `json:"smoke_sha256"`
	CategorySchema   string `json:"category_schema"`
	CaseCount        uint64 `json:"case_count"`
}

func main() {
	if len(os.Args) != 3 || os.Args[1] == "" || os.Args[2] == "" {
		fmt.Fprintln(os.Stderr, "usage: codex-stage11-provenance <cq-build> <reviewed-manifest>")
		os.Exit(2)
	}
	data, err := os.ReadFile(os.Args[2])
	if err != nil {
		panic(err)
	}
	var manifest reviewedManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		panic(err)
	}
	schema := sha256.Sum256([]byte(manifest.CategorySchema))
	fields := []string{
		"cq-codex-stage11-build-provenance-v1",
		os.Args[1],
		manifest.Revision,
		manifest.TranscriptSHA256,
		manifest.SmokeSHA256,
		hex.EncodeToString(schema[:]),
		fmt.Sprint(manifest.CaseCount),
	}
	hash := sha256.New()
	for index, field := range fields {
		if index > 0 {
			_, _ = hash.Write([]byte{0})
		}
		_, _ = hash.Write([]byte(field))
	}
	fmt.Printf("%x\n", hash.Sum(nil))
}
