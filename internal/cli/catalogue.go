package cli

func Help(path string) (string, bool) {
	help, ok := helpByPath[path]
	return help, ok
}

func Completion(shell string) (string, bool) {
	script, ok := completionByShell[shell]
	return script, ok
}
