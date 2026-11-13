package hostinfo

// Space of the volume holding a path, VolumeUsed set on macOS where the volume reports its own share
type Disk struct {
	Total      uint64
	Used       uint64
	Free       uint64
	VolumeUsed *uint64
}

// Share of the volume a user can no longer fill in percent, the volume's own use where reported
func (d Disk) Percent() float64 {
	used := d.Used
	if d.VolumeUsed != nil {
		used = *d.VolumeUsed
	}
	if used+d.Free == 0 {
		return 0
	}
	return roundDecimals(float64(used)/float64(used+d.Free)*100, 1)
}
