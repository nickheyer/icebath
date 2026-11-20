package selfupdate

func gitArgs(gitCmd []string, args ...string) []string {
	return append(append([]string{}, gitCmd...), args...)
}
