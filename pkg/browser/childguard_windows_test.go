//go:build windows

package browser

// Opens no root pipe, the child mode cases being posix only like their hermes platforms marks
func startChildGuard() func() { return func() {} }
