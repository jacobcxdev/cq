package cli

import (
	"encoding/json"
	"fmt"
	"regexp"
)

const cliSchemaVersion = 2

var (
	semverPattern   = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-(?:0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*))*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
	revisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

type versionData struct {
	Version          string  `json:"version"`
	Revision         *string `json:"revision"`
	Dirty            *bool   `json:"dirty"`
	CLISchemaVersion int     `json:"cli_schema_version"`
}

type completionData struct {
	Shell  string `json:"shell"`
	Script string `json:"script"`
}

func VersionOutcome(info BuildInfo) Outcome {
	version := info.Version
	if !semverPattern.MatchString(version) {
		version = "dev"
	}
	buildRevision := info.Revision
	if buildRevision != nil && !revisionPattern.MatchString(*buildRevision) {
		buildRevision = nil
	}
	revision := "unknown"
	if buildRevision != nil {
		revision = *buildRevision
	}
	data, err := json.Marshal(versionData{
		Version:          version,
		Revision:         buildRevision,
		Dirty:            info.Dirty,
		CLISchemaVersion: cliSchemaVersion,
	})
	if err != nil {
		panic(err)
	}
	return Outcome{
		ExitCode: 0,
		Data:     data,
		Human:    fmt.Sprintf("cq %s\nRevision: %s\nCLI schema: %d\n", version, revision, cliSchemaVersion),
	}
}

func CompletionOutcome(shell string) Outcome {
	script, ok := Completion(shell)
	if !ok {
		return Outcome{
			ExitCode: 2,
			Errors: []Diagnostic{{
				Code:    "invalid_argument",
				Message: "Invalid shell: expected one of bash, zsh, fish.",
			}},
		}
	}
	data, err := json.Marshal(completionData{Shell: shell, Script: script})
	if err != nil {
		panic(err)
	}
	return Outcome{ExitCode: 0, Data: data, Human: script}
}
