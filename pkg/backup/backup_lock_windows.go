package backup

import "os"

// Gives an empty lock file the byte msvcrt.locking needs before the first byte can be locked
func prepareLockFile(lockPath string, handle *os.File) error {
	st, err := os.Stat(lockPath)
	if err != nil {
		return err
	}
	if st.Size() == 0 {
		if _, err := handle.Write([]byte(" ")); err != nil {
			return err
		}
	}
	return nil
}
