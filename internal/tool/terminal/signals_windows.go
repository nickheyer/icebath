package terminal

var signalNames = map[int]string{
	1: "CTRL_BREAK_EVENT", 2: "SIGINT", 4: "SIGILL", 8: "SIGFPE", 11: "SIGSEGV", 15: "SIGTERM", 21: "SIGBREAK", 22: "SIGABRT",
}

func signalName(signum int) (string, bool) {
	name, ok := signalNames[signum]
	return name, ok
}
