//go:build !((darwin || freebsd || linux || netbsd) && !faketime)

package gateway

// GlibcMallocHandle is the mallopt entry point of a glibc process
type GlibcMallocHandle struct{}

// No process global symbol namespace exists here, so no glibc handle
func GlibcMalloc() *GlibcMallocHandle { return nil }

// Unreachable without a handle, mallopt reports failure
func (h *GlibcMallocHandle) Mallopt(param, value int32) int32 { return 0 }
