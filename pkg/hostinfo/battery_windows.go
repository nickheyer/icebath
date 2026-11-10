package hostinfo

import (
	"syscall"
	"unsafe"
)

type systemPowerStatus struct {
	ACLineStatus        byte
	BatteryFlag         byte
	BatteryLifePercent  byte
	SystemStatusFlag    byte
	BatteryLifeTime     uint32
	BatteryFullLifeTime uint32
}

var procGetSystemPowerStatus = syscall.NewLazyDLL("kernel32.dll").NewProc("GetSystemPowerStatus")

// Reads the system power status the way psutil sensors_battery does on Windows
func platformSensorsBattery() (sensorReading, bool) {
	var status systemPowerStatus
	if ok, _, _ := procGetSystemPowerStatus.Call(uintptr(unsafe.Pointer(&status))); ok == 0 {
		return sensorReading{}, false
	}
	if status.BatteryFlag&128 != 0 {
		return sensorReading{}, false
	}
	percent := float64(status.BatteryLifePercent)
	plugged := status.ACLineStatus == 1
	return sensorReading{percent: &percent, plugged: &plugged}, true
}
