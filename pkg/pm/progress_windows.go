//go:build windows

package pm

// Name Python's signal module gives a signal number on Windows, empty when the number has none
func signalName(n int) string {
	switch n {
	case 2:
		return "SIGINT"
	case 4:
		return "SIGILL"
	case 8:
		return "SIGFPE"
	case 11:
		return "SIGSEGV"
	case 15:
		return "SIGTERM"
	case 21:
		return "SIGBREAK"
	case 22:
		return "SIGABRT"
	}
	return ""
}
