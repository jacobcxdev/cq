//go:build linux

package proxy

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRuntimeWorkerDiesWithSupervisorParent(t *testing.T) {
	const modeName = "CQ_TEST_LINUX_WORKER_PARENT_DEATH"
	switch os.Getenv(modeName) {
	case "parent":
		command := exec.Command(os.Args[0], "-test.run=^TestRuntimeWorkerDiesWithSupervisorParent$")
		command.Env = runtimeWorkerParentDeathTestEnvironment(modeName, "child")
		configureRuntimeWorkerCommand(command)
		if err := command.Start(); err != nil {
			os.Exit(2)
		}
		fmt.Println(command.Process.Pid)
		_ = os.Stdout.Sync()
		os.Exit(0)
	case "child":
		for {
			time.Sleep(time.Hour)
		}
	}

	parent := exec.Command(os.Args[0], "-test.run=^TestRuntimeWorkerDiesWithSupervisorParent$")
	parent.Env = runtimeWorkerParentDeathTestEnvironment(modeName, "parent")
	output, err := parent.Output()
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil || pid <= 1 {
		t.Fatalf("child pid = %q, %v", output, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); errors.Is(err, os.ErrNotExist) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("runtime worker %d survived supervisor exit", pid)
}

func runtimeWorkerParentDeathTestEnvironment(name, value string) []string {
	environment := make([]string, 0, len(os.Environ())+1)
	prefix := name + "="
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, prefix) {
			environment = append(environment, variable)
		}
	}
	return append(environment, prefix+value)
}
