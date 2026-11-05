package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/harness"
)

type CodexHarnessSkillCmd struct {
	Install struct {
		Path string `help:"Absolute curate-harness skill directory (default: CODEX_HOME/skills/curate-harness)"`
	} `cmd:"" help:"Install or update CQ-owned curation instructions"`
}

func runHarnessSkillCommand(operation string, cmd CodexHarnessCmd, jsonOutput bool, output io.Writer) error {
	if operation != "skill install" {
		return fmt.Errorf("unknown harness skill operation")
	}
	path := cmd.Skill.Install.Path
	if path == "" {
		var err error
		path, err = harness.CurationSkillPath()
		if err != nil {
			return err
		}
	}
	result, err := harness.InstallCurationSkill(fsutil.OSFileSystem{}, path)
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(output).Encode(result)
	}
	if result.Changed {
		_, err = fmt.Fprintf(output, "Installed curation skill: %s\n", result.Path)
	} else {
		_, err = fmt.Fprintf(output, "Curation skill already current: %s\n", result.Path)
	}
	return err
}
