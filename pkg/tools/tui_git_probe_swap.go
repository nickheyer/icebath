package tools

// SwapGitProbeRunner replaces the git command of the root probes with an empty cache and returns the restore
func SwapGitProbeRunner(fn func(cwd string, args ...string) string) (restore func()) {
	saved := setTUIRunGit(fn)
	GitProbeInvalidate()
	return func() {
		setTUIRunGit(saved)
		GitProbeInvalidate()
	}
}
