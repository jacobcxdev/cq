package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func runNativeProxyEntry(args []string, stdout io.Writer) (bool, error) {
	if len(args) == 0 || args[0] != "__native-proxy" {
		return false, nil
	}
	if len(args) != 2 || args[1] != "descriptor-path" {
		return true, errors.New("native proxy helper requires descriptor-path")
	}
	path, err := proxy.NativeEgressDescriptorPath(fsutil.OSFileSystem{})
	if err != nil {
		return true, err
	}
	return true, json.NewEncoder(stdout).Encode(struct {
		Path string `json:"path"`
		UID  int    `json:"uid"`
	}{path, os.Getuid()})
}
