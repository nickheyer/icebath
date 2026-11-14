package local

import "github.com/nickheyer/icebath/internal/hostinfo"

func ramBytes(host *hostinfo.Host) (int64, int64) {
	total, err := host.RAMTotal()
	if err != nil || total == 0 {
		return 0, 0
	}
	available := int64(total / 2)
	if m, err := hostinfo.ReadMemory(); err == nil {
		pages := m.Available
		if m.Purgeable != nil {
			pages += *m.Purgeable
		}
		if pages > 0 {
			available = int64(pages)
		}
	}
	return int64(total), available
}
