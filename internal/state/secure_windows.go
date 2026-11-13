package state

// Windows files carry no POSIX mode to tighten, a database inheriting the ACL of its directory
func (e *Engine) SecureDBFiles(string, bool) error { return nil }
