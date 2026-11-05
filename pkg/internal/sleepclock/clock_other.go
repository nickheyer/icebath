//go:build !windows

package sleepclock

func awakeClock() func() float64 { return monotonicSeconds }
