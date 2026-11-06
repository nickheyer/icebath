//go:build unix && !linux

package gateway

const rlimInfinity = uint64(1<<63 - 1)
