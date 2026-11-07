package state

// Restricts state.db and its WAL sidecars to the owner, creating the main file first when createMain
func SecureStateDBFiles(dbPath string, createMain bool) error {
	return secureStateDBFiles(dbPath, createMain)
}
