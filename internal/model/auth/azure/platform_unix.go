//go:build !windows

package azure

import (
	"bytes"
	"unsafe"

	"github.com/nickheyer/icebath/internal/dl"
	"github.com/nickheyer/icebath/internal/hostinfo"
)

var powerShells = []string{"pwsh"}

func safeWorkingDir() (string, error) { return "/bin", nil }

func brokerSupported(host *hostinfo.Host) bool { return host != nil && host.WSL() }

type osString []byte

func encodeOS(s string) osString { return append([]byte(s), 0) }

func newOSBuffer(n int32) osString { return make(osString, n) }

func (o osString) pointer() unsafe.Pointer { return unsafe.Pointer(&o[0]) }

func (o osString) text() string {
	if i := bytes.IndexByte(o, 0); i >= 0 {
		return string(o[:i])
	}
	return string(o)
}

func osText(addr uintptr) string { return dl.GoString(addr) }
