//go:build (darwin || freebsd || linux || netbsd) && !faketime

package gateway

import "github.com/ebitengine/purego"

// GlibcMallocHandle is the mallopt entry point of a glibc process
type GlibcMallocHandle struct{ mallopt uintptr }

// glibc libc handle of this process when it exports gnu_get_libc_version and mallopt, nil otherwise
func GlibcMalloc() *GlibcMallocHandle {
	if sym, err := purego.Dlsym(purego.RTLD_DEFAULT, "gnu_get_libc_version"); err != nil || sym == 0 {
		return nil
	}
	mallopt, err := purego.Dlsym(purego.RTLD_DEFAULT, "mallopt")
	if err != nil || mallopt == 0 {
		return nil
	}
	return &GlibcMallocHandle{mallopt: mallopt}
}

// Calls mallopt with a C int parameter and value, returning its C int result
func (h *GlibcMallocHandle) Mallopt(param, value int32) int32 {
	r, _, _ := purego.SyscallN(h.mallopt, uintptr(param), uintptr(value))
	return int32(r)
}
