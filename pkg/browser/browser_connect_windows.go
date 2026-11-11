//go:build windows

package browser

import (
	"strconv"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Effective ProgId of a URL scheme as the Windows Shell resolves it, empty when the query fails
func windowsShellProgID(scheme string) string {
	query := windows.NewLazySystemDLL("shlwapi.dll").NewProc("AssocQueryStringW")
	if query.Find() != nil {
		return ""
	}
	schemePtr, err := windows.UTF16PtrFromString(scheme)
	if err != nil {
		return ""
	}
	buf := make([]uint16, 256)
	size := uint32(len(buf))
	status, _, _ := query.Call(0, 20, uintptr(unsafe.Pointer(schemePtr)), 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if int32(status) != 0 {
		return ""
	}
	return windows.UTF16ToString(buf)
}

// ProgId text of an open association key, absent when the value cannot be read
func registryProgIDText(key registry.Key) (string, bool) {
	if text, _, err := key.GetStringValue("ProgId"); err == nil {
		return text, true
	}
	if number, _, err := key.GetIntegerValue("ProgId"); err == nil {
		if number == 0 {
			return "", true
		}
		return strconv.FormatUint(number, 10), true
	}
	if data, _, err := key.GetBinaryValue("ProgId"); err == nil {
		if len(data) == 0 {
			return "", true
		}
		return pyrepr.BytesRepr(data), true
	}
	if list, _, err := key.GetStringsValue("ProgId"); err == nil {
		if len(list) == 0 {
			return "", true
		}
		items := make([]any, 0, len(list))
		for _, s := range list {
			items = append(items, s)
		}
		return pyrepr.Repr(items), true
	}
	return "", false
}

// Registry ProgId of the https association, the UserChoiceLatest key read before the legacy UserChoice
func windowsRegistryProgID() string {
	for _, sub := range []string{`UserChoiceLatest\ProgId`, "UserChoice"} {
		key, err := registry.OpenKey(registry.CURRENT_USER, windowsHTTPSAssoc+`\`+sub, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		progID, ok := registryProgIDText(key)
		key.Close()
		if ok && progID != "" {
			return progID
		}
	}
	return ""
}
