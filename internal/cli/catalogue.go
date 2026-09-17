package cli

func Help(path string) (string, bool) {
	if help, ok := compatibilityHelp(path); ok {
		return help, true
	}
	help, ok := helpByPath[path]
	return help, ok
}

func Completion(shell string) (string, bool) {
	script, ok := completionByShell[shell]
	return script, ok
}
