//go:build windows

package gateway

import "errors"

const rlimInfinity = ^uint64(0)

var nofileSupported = false

var errNoResourceModule = errors.New("No module named 'resource'")

var getNofile = func() (uint64, uint64, error) { return 0, 0, errNoResourceModule }

var setNofile = func(soft, hard uint64) error { return errNoResourceModule }
