//go:build !linux && !darwin && !windows

package tui

// Resident set size of the process, unknown on a system without a probe for it
func residentBytes() uint64 { return 0 }

// Resident bytes of the process that no file backs, unknown on a system without a probe for them
func anonymousBytes() uint64 { return 0 }

// Open file descriptors of the process, unknown on a system without a probe for them
func openDescriptors() (int, bool) { return 0, false }

// Peak resident size and CPU times of the process, unknown on a system without a probe for them
func resourceUsage() (processUsage, bool) { return processUsage{}, false }

// Physical memory of the machine, unknown on a system without a probe for it
func physicalMemory() uint64 { return 0 }

// Memory limit of a control group, none on a system without control groups
func cgroupMemoryLimit() uint64 { return 0 }

// Memory map totals of the process, empty on a system without procfs
func smapsRollup() string { return "" }
