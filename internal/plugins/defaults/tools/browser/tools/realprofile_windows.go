//go:build windows

package tools

import (
	"fmt"
	"strconv"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const httpsAssociation = `Software\Microsoft\Windows\Shell\Associations\UrlAssociations\https`

// ProgId of a URL scheme as the Windows Shell resolves it, empty when the query fails
func shellProgID(scheme string) string {
	query := windows.NewLazySystemDLL("shlwapi.dll").NewProc("AssocQueryStringW")
	if query.Find() != nil {
		return ""
	}
	name, err := windows.UTF16PtrFromString(scheme)
	if err != nil {
		return ""
	}
	buf := make([]uint16, 256)
	size := uint32(len(buf))
	status, _, _ := query.Call(0, 20, uintptr(unsafe.Pointer(name)), 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if int32(status) != 0 {
		return ""
	}
	return windows.UTF16ToString(buf)
}

// ProgId value of an open association key as text, false when it can't be read
func progIDText(key registry.Key) (string, bool) {
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
		return strconv.Quote(string(data)), true
	}
	if list, _, err := key.GetStringsValue("ProgId"); err == nil {
		if len(list) == 0 {
			return "", true
		}
		return fmt.Sprintf("%q", list), true
	}
	return "", false
}

// ProgId of the https association in the registry, UserChoiceLatest read before the older UserChoice
func registryProgID() string {
	for _, sub := range []string{`UserChoiceLatest\ProgId`, "UserChoice"} {
		key, err := registry.OpenKey(registry.CURRENT_USER, httpsAssociation+`\`+sub, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		progID, ok := progIDText(key)
		key.Close()
		if ok && progID != "" {
			return progID
		}
	}
	return ""
}
