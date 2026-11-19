package app

// Prints the canonical version block with the update check, assigned by the binary at startup
var printVersionInfoHook func(checkUpdates bool)

// Sets the version printer the console's version command runs
func SetPrintVersionInfo(fn func(checkUpdates bool)) { printVersionInfoHook = fn }
