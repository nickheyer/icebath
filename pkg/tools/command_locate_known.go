package tools

// Argv of the first present PATH or known directory candidate for a command name, nil when it is not found
func LocateCommandKnown(name string, knownDirs []string) []string {
	return locateCommand(name, nil, knownDirs).Command()
}
