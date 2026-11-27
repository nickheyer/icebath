//go:build (((darwin || freebsd || netbsd) && (amd64 || arm64)) || (linux && (cgo || !android) && (amd64 || arm64 || loong64 || ppc64le || riscv64 || (cgo && s390x))) || (windows && (amd64 || arm64))) && !faketime

package native

import "github.com/ebitengine/purego"

// C function pointer that calls a Go function, never released; an error once the process has made as many as the platform allows
func NewCallback(fn any) (cb uintptr, err error) {
	defer func() {
		if r := recover(); r != nil {
			cb, err = 0, &callbackError{reason: r}
		}
	}()
	return purego.NewCallback(fn), nil
}
