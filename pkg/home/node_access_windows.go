package home

// Reports true for any existing path since Windows grants execute through the file type alone
func executableByProcess(string) bool { return true }
