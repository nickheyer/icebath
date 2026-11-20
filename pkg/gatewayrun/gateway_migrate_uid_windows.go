package gatewayrun

import "os"

// Owner uid of a stat result, always zero on Windows the way os.stat reports it
func statUID(os.FileInfo) (int, bool) { return 0, true }
