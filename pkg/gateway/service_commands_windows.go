package gateway

// Windows paths carry no uid, so ownership is never established
func fileOwnerUID(p string) (int, bool) { return 0, false }
