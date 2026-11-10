//go:build !linux && !windows && !freebsd && !(darwin && cgo)

package hostinfo

// IOKit needs cgo and the remaining platforms expose no battery sensor, so the reading is unavailable
func platformSensorsBattery() (sensorReading, bool) { return sensorReading{}, false }
