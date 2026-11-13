package proc

// Process table read from the native system process list, which also covers processes OpenProcess refuses
func readTable() (Table, error) {
	all, err := systemProcesses()
	if err != nil {
		return nil, err
	}
	t := make(Table, 0, len(all))
	for _, p := range all {
		st := Running
		if p.suspended {
			st = Stopped
		}
		t = append(t, Entry{PID: p.pid, PPID: p.ppid, Start: filetimeSeconds(p.create), Status: st})
	}
	return t, nil
}
