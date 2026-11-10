package hostinfo

import "golang.org/x/sys/unix"

// Reads the ACPI battery sysctls the way psutil sensors_battery does on FreeBSD
func platformSensorsBattery() (sensorReading, bool) {
	life, err := unix.SysctlUint32("hw.acpi.battery.life")
	if err != nil {
		return sensorReading{}, false
	}
	if _, err := unix.SysctlUint32("hw.acpi.battery.time"); err != nil {
		return sensorReading{}, false
	}
	acline, err := unix.SysctlUint32("hw.acpi.acline")
	if err != nil {
		return sensorReading{}, false
	}
	percent := float64(int32(life))
	plugged := acline == 1
	return sensorReading{percent: &percent, plugged: &plugged}, true
}
