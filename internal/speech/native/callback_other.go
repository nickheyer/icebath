//go:build !((((darwin || freebsd || netbsd) && (amd64 || arm64)) || (linux && (cgo || !android) && (amd64 || arm64 || loong64 || ppc64le || riscv64 || (cgo && s390x))) || (windows && (amd64 || arm64))) && !faketime)

package native

// Always ErrNoCallback, this build has no trampoline for C to call into Go
func NewCallback(fn any) (uintptr, error) { return 0, ErrNoCallback }
