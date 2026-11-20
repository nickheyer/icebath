//go:build !linux && !windows && !freebsd && !darwin

package hostinfo

// The remaining platforms expose no battery sensor, so the reading is unavailable
func platformSensorsBattery() (sensorReading, bool) { return sensorReading{}, false }
