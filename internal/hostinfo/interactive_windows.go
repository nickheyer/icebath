package hostinfo

import "golang.org/x/sys/windows"

const noConsoleSession = 0xFFFFFFFF

var procGetProcessWindowStation = windows.NewLazySystemDLL("user32.dll").NewProc("GetProcessWindowStation")

// Reports a process outside session zero that sits on the physical console or owns a window station
func InteractiveSession() bool {
	var session uint32
	if windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &session) != nil || session == 0 {
		return false
	}
	if active := windows.WTSGetActiveConsoleSessionId(); active != noConsoleSession && active == session {
		return true
	}
	if procGetProcessWindowStation.Find() != nil {
		return false
	}
	station, _, _ := procGetProcessWindowStation.Call()
	return station != 0
}
