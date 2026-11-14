package local

import "github.com/nickheyer/icebath/internal/hostinfo"

func ramBytes(*hostinfo.Host) (int64, int64) {
	m, err := hostinfo.ReadMemory()
	if err != nil {
		return 0, 0
	}
	return int64(m.Total), int64(m.Available)
}
