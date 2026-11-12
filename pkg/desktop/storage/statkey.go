package storage

import (
	"errors"
	"io/fs"
	"os"

	"github.com/nickheyer/icebath/pkg/desktop/hardening"
)

// Stat fields a file poll compares, the error kind standing in for a missing file
type statKey struct {
	info fs.FileInfo
	err  string
}

func statKeyOf(file string) statKey {
	info, err := os.Stat(file)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return statKey{err: "ENOENT"}
		}
		return statKey{err: hardening.NodeCode(err)}
	}
	return statKey{info: info}
}

// True when two polls saw the same file identity, size, mode and modification time
func sameStat(a, b statKey) bool {
	if a.info == nil || b.info == nil {
		return a.info == nil && b.info == nil && a.err == b.err
	}
	return os.SameFile(a.info, b.info) && a.info.Size() == b.info.Size() && a.info.Mode() == b.info.Mode() &&
		a.info.ModTime().Equal(b.info.ModTime())
}
