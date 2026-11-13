package proc

import (
	"os"
	"strconv"
)

// Processes from every numeric procfs stat file, those exiting mid read skipped, pgid zero meaning every group
func scanProc(pgid int) (Table, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	t := make(Table, 0, len(entries))
	for _, de := range entries {
		pid, err := strconv.Atoi(de.Name())
		if err != nil || pid <= 0 {
			continue
		}
		fields, err := statFields(pid)
		if err != nil {
			continue
		}
		if pgid != 0 && fields[2] != itoa(pgid) {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		ticks, err := strconv.ParseUint(fields[19], 10, 64)
		if err != nil {
			continue
		}
		t = append(t, Entry{PID: pid, PPID: ppid, Start: float64(ticks) / clockTicks(), Status: statusLetter(fields[0][0])})
	}
	return t, nil
}

// Process table of every numeric procfs entry
func readTable() (Table, error) { return scanProc(0) }

// Members of process group pgid
func groupMembers(pgid int) (Table, error) { return scanProc(pgid) }
