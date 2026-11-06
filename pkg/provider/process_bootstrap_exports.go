package provider

// Ignores SIGPIPE at agent startup so a vanished stdout or stderr pipe never kills the process
func InstallSafeStdio() { installSafeStdio() }
