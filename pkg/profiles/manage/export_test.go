package manage

// Fixture helpers of the package tests that the external test package of this directory also calls
var (
	ManageEnv      = manageEnv
	WriteFile      = writeFile
	ArchiveMembers = archiveMembers
	Exists         = exists
	CaptureOut     = captureOut
	ReadText       = readText
)

// Installs a remover for staged runtime directories and returns the one it replaced
func SwapRmtreeStagedRuntime(f func(string) error) func(string) error {
	old := rmtreeStagedRuntime
	rmtreeStagedRuntime = f
	return old
}
