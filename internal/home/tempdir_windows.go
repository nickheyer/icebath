package home

// Temp directories a Windows host falls back to after the temp variables, the profile and system ones first
func platformTempDirs() []string {
	return []string{ExpandUser(`~\AppData\Local\Temp`), expandReferences(`%SYSTEMROOT%\Temp`), `c:\temp`, `c:\tmp`, `\temp`, `\tmp`}
}
