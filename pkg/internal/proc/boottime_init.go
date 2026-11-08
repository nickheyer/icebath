//go:build unix && !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package proc

// Boot epoch in seconds like psutil boot_time, the start of init as the utmpx boot record states it
func BootTime() (float64, error) { return Birth(1) }
