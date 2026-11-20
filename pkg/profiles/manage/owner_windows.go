package manage

// Owner uid of a path, false on Windows which has no getuid
func fileOwnerUID(string) (int, bool) { return 0, false }
