package pyfs

import (
	"errors"
	"io/fs"
	"os"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// os.makedirs creating missing parents first, an existing directory accepted when existOK
func Makedirs(name string, mode os.FileMode, existOK bool) error {
	head, tail := pypath.Split(name)
	if tail == "" {
		head, tail = pypath.Split(head)
	}
	if head != "" && tail != "" {
		if _, err := os.Stat(head); err != nil {
			if err := Makedirs(head, 0o777, existOK); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			if tail == "." {
				return nil
			}
		}
	}
	if err := os.Mkdir(name, mode); err != nil {
		if !existOK || !isDirPath(name) {
			return err
		}
	}
	return nil
}
