//go:build unix

package state

import (
	"math/big"

	"golang.org/x/sys/unix"
)

// The os.makedev attribute, composing a device number with this kernel's encoding
func osMakedev() (func(major, minor *big.Int) (uint64, error), error) {
	return func(major, minor *big.Int) (uint64, error) {
		maj, mnr, err := makedevParts(major, minor)
		if err != nil {
			return 0, err
		}
		return unix.Mkdev(maj, mnr), nil
	}, nil
}
