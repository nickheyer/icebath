package home

// Windows files carry no POSIX owner, so ICEBATH_UID and ICEBATH_GID never apply
func chownToConfigured(string) {}
