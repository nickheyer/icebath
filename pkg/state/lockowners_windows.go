package state

import "math/big"

// Windows Python has no os.makedev, so looking it up raises its AttributeError before any argument is parsed
func osMakedev() (func(major, minor *big.Int) (uint64, error), error) {
	return nil, &pyConvError{"AttributeError", "module 'os' has no attribute 'makedev'"}
}
