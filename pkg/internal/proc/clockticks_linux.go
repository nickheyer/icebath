package proc

// Kernel clock ticks per second, the SC_CLK_TCK psutil divides CPU and start times by
func ClockTicks() float64 { return clockTicksPerSecond() }
