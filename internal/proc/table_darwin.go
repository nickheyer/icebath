package proc

import "golang.org/x/sys/unix"

// Table of the kernel's kinfo_proc records under one sysctl name
func kinfoTable(name string, args ...int) (Table, error) {
	procs, err := unix.SysctlKinfoProcSlice(name, args...)
	if err != nil {
		return nil, err
	}
	t := make(Table, 0, len(procs))
	for _, kp := range procs {
		t = append(t, Entry{
			PID:    int(kp.Proc.P_pid),
			PPID:   int(kp.Eproc.Ppid),
			Start:  float64(kp.Proc.P_starttime.Sec) + float64(kp.Proc.P_starttime.Usec)/1e6,
			Status: darwinStatus(kp.Proc.P_stat),
		})
	}
	return t, nil
}

// Process table read from the kernel's kinfo_proc list
func readTable() (Table, error) { return kinfoTable("kern.proc.all") }

// Members of process group pgid
func groupMembers(pgid int) (Table, error) { return kinfoTable("kern.proc.pgrp", pgid) }
