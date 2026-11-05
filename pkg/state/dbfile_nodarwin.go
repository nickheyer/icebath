//go:build !darwin

package state

// libproc lookup on a host without it, failing the way the missing symbol lookup fails
func loadDarwinLibproc() (darwinLibprocAPI, error) {
	return nil, &pyConvError{"AttributeError", "undefined symbol: proc_listpids"}
}
