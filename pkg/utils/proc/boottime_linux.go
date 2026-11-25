package proc

// Boot epoch in seconds like psutil boot_time, read from /proc/stat btime
func BootTime() (float64, error) { return bootTime() }
