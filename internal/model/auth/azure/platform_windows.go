package azure

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"unicode/utf16"
	"unsafe"

	"github.com/nickheyer/icebath/internal/dl"
	"github.com/nickheyer/icebath/internal/hostinfo"
)

const osTextLimit = 1 << 16

var powerShells = []string{"pwsh", "powershell"}

func safeWorkingDir() (string, error) {
	if root := os.Getenv("SYSTEMROOT"); root != "" {
		return root, nil
	}
	return "", errors.New("environment variable SYSTEMROOT has no value")
}

func arcTokensDir() string {
	data := os.Getenv("ProgramData")
	if data == "" {
		return ""
	}
	return filepath.Join(data, "AzureConnectedMachineAgent", "Tokens")
}

func brokerSupported(*hostinfo.Host) bool { return true }

func browserCommand(target string, _ *hostinfo.Host) *exec.Cmd {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
}

type osString []uint16

func encodeOS(s string) osString { return append(utf16.Encode([]rune(s)), 0) }

func newOSBuffer(n int32) osString { return make(osString, n) }

func (o osString) pointer() unsafe.Pointer { return unsafe.Pointer(&o[0]) }

func (o osString) text() string {
	for i, unit := range o {
		if unit == 0 {
			return string(utf16.Decode(o[:i]))
		}
	}
	return string(utf16.Decode(o))
}

func osText(addr uintptr) string {
	if addr == 0 {
		return ""
	}
	var units []uint16
	for i := uintptr(0); i < osTextLimit; i++ {
		unit := *dl.At[uint16](addr + 2*i)
		if unit == 0 {
			break
		}
		units = append(units, unit)
	}
	return string(utf16.Decode(units))
}
