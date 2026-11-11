package selfupdate

import "os"

const openNoFollow = 0

// Sets the read only state of st on the open file itself, the one mode bit Windows keeps
func chmodHandle(out *os.File, st os.FileInfo) error { return out.Chmod(st.Mode().Perm()) }

// Leaves the times alone, Windows setting them only through a path
func utimeHandle(*os.File, os.FileInfo) error { return nil }
