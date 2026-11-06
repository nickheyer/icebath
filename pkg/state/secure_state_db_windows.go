package state

// Leaves Windows ACL inherited state database files as created, the permission bits carrying no meaning there
func secureStateDBFiles(dbPath string, createMain bool) error { return nil }
