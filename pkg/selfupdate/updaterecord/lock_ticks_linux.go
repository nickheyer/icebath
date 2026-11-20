package updaterecord

import "github.com/nickheyer/icebath/pkg/internal/proc"

// Clock ticks per second sysconf SC_CLK_TCK reports, read from the auxiliary vector
func clockTicks() float64 { return proc.ClockTicks() }
