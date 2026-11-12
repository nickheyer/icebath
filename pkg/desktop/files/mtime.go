package files

import "io/fs"

// Modification time in milliseconds with the fraction Node keeps in mtimeMs
func mtimeMs(st fs.FileInfo) float64 {
	ns := st.ModTime().UnixNano()
	seconds, nanos := ns/1_000_000_000, ns%1_000_000_000
	if nanos < 0 {
		seconds, nanos = seconds-1, nanos+1_000_000_000
	}
	return float64(seconds)*1e3 + float64(nanos)/1e6
}
