//go:build (freebsd || linux || netbsd) && !386 && !arm

package sysproxy

import (
	"errors"
	"net/url"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// GIO proxy resolver entry points, the resolver WebKitGTK itself routes requests through
var gio struct {
	once       sync.Once
	err        error
	getDefault func() unsafe.Pointer
	lookup     func(resolver unsafe.Pointer, uri string, cancellable unsafe.Pointer, errp *unsafe.Pointer) unsafe.Pointer
	strfreev   func(unsafe.Pointer)
	errorFree  func(unsafe.Pointer)
}

func loadGIO() error {
	gio.once.Do(func() {
		lib, err := purego.Dlopen("libgio-2.0.so.0", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			gio.err = err
			return
		}
		purego.RegisterLibFunc(&gio.getDefault, lib, "g_proxy_resolver_get_default")
		purego.RegisterLibFunc(&gio.lookup, lib, "g_proxy_resolver_lookup")
		purego.RegisterLibFunc(&gio.strfreev, lib, "g_strfreev")
		purego.RegisterLibFunc(&gio.errorFree, lib, "g_error_free")
	})
	return gio.err
}

// Copies a NUL terminated C string
func cString(p unsafe.Pointer) string {
	var out []byte
	for {
		b := *(*byte)(p)
		if b == 0 {
			return string(out)
		}
		out = append(out, b)
		p = unsafe.Add(p, 1)
	}
}

// Proxy URIs the default GIO resolver answers for target, mapped to PAC entries
func resolve(target *url.URL) (string, error) {
	if err := loadGIO(); err != nil {
		return "", err
	}
	resolver := gio.getDefault()
	if resolver == nil {
		return "", errors.New("GIO has no default proxy resolver")
	}
	var gerr unsafe.Pointer
	list := gio.lookup(resolver, target.String(), nil, &gerr)
	if list == nil {
		message := "GIO proxy lookup failed"
		if gerr != nil {
			message = cString(*(*unsafe.Pointer)(unsafe.Add(gerr, 8)))
			gio.errorFree(gerr)
		}
		return "", errors.New(message)
	}
	defer gio.strfreev(list)
	var entries []string
	for i := 0; ; i++ {
		item := *(*unsafe.Pointer)(unsafe.Add(list, i*int(unsafe.Sizeof(uintptr(0)))))
		if item == nil {
			break
		}
		entries = append(entries, pacEntry(cString(item)))
	}
	return pacAnswer(entries), nil
}
