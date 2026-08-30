package proxy

import (
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

func linuxProxyRuntimeArgumentsMatch(arguments []string, executable string) bool {
	return len(arguments) == 3 && filepath.IsAbs(executable) && filepath.Clean(executable) == executable &&
		arguments[0] == executable && arguments[1] == "proxy" && arguments[2] == "start"
}

func linuxProxyRuntimeCgroupMatches(cgroup string, uid uint64) bool {
	if cgroup == "" || !strings.HasPrefix(cgroup, "/") || path.Clean(cgroup) != cgroup || path.Base(cgroup) != "cq-proxy.service" {
		return false
	}
	uidText := strconv.FormatUint(uid, 10)
	userManager := "/user.slice/user-" + uidText + ".slice/user@" + uidText + ".service/"
	return strings.Contains(cgroup, userManager) && strings.HasSuffix(cgroup, "/cq-proxy.service")
}
