package gateway

// Whether stdin can answer a prompt, a tty that also passes the Windows console mode check
func StdinIsInteractive(isatty bool, consoleModeOK *bool) bool {
	return stdinIsInteractive(isatty, consoleModeOK)
}

// Windows console mode verdict of stdin, nil off Windows where isatty alone decides
func StdinConsoleModeOK() *bool { return stdinConsoleModeOK() }
