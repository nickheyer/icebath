//go:build windows

package proc

import (
	"time"

	"golang.org/x/sys/windows"
)

var procGetTickCount64 = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetTickCount64")

// Boot epoch in whole seconds like psutil boot_time, now minus the GetTickCount64 uptime
func BootTime() (float64, error) {
	if err := procGetTickCount64.Find(); err != nil {
		return 0, err
	}
	ticks, _, _ := procGetTickCount64.Call()
	return float64(time.Now().Unix() - int64(ticks/1000)), nil
}
