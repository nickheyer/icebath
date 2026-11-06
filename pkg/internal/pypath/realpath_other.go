//go:build !windows

package pypath

// ntpath.abspath where nt._getfullpathname is missing, the working directory joined and normalized
func ntAbspath(p string) (string, error) {
	if !NtIsAbs(p) {
		cwd, err := Getcwd()
		if err != nil {
			return "", err
		}
		p = NtJoin(cwd, p)
	}
	return NtNormpath(p), nil
}

// ntpath.realpath where nt._getfinalpathname is missing, which CPython reduces to abspath
func ntRealpath(p string, _ Strict) (string, error) { return ntAbspath(p) }
