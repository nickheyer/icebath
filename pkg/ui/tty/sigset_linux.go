//go:build linux && !mips && !mipsle && !mips64 && !mips64le

package tty

// Bytes of the kernel signal set, 64 signals
const sigsetSize = 8
