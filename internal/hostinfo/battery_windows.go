package hostinfo

import (
	"fmt"
	"unsafe"
)

const (
	batteryFlagNoBattery = 128
	acLineOnline         = 1
)

var procGetSystemPowerStatus = kernel32.NewProc("GetSystemPowerStatus")

// Power status GetSystemPowerStatus fills, the fields nothing reads left blank
type systemPowerStatus struct {
	acLineStatus       byte
	batteryFlag        byte
	batteryLifePercent byte
	_                  byte
	_                  uint32
	_                  uint32
}

// System power status charge byte as reported, external power only when the AC line reads online
func sensorsBattery() (batteryReading, error) {
	if err := procGetSystemPowerStatus.Find(); err != nil {
		return batteryReading{}, fmt.Errorf("battery: %w", err)
	}
	var status systemPowerStatus
	if r, _, err := procGetSystemPowerStatus.Call(uintptr(unsafe.Pointer(&status))); r == 0 {
		return batteryReading{}, fmt.Errorf("battery: %w", err)
	}
	if status.batteryFlag&batteryFlagNoBattery != 0 {
		return batteryReading{}, fmt.Errorf("battery: %w", ErrAbsent)
	}
	plugged := status.acLineStatus == acLineOnline
	return batteryReading{percent: float64(status.batteryLifePercent), plugged: &plugged}, nil
}
