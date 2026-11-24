package charsetutil

import (
	"strconv"

	"golang.org/x/sys/windows"
)

var procGetOEMCP = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetOEMCP")

// Canonical name of the process locale's character set, the ANSI code page, UTF-8 when this package does not know that code page
func Locale() string {
	if cs, ok := find("cp" + strconv.FormatUint(uint64(windows.GetACP()), 10)); ok {
		return cs.name
	}
	return "UTF-8"
}

// Code page label mbcs, ansi and dbcs stand for, the ANSI code page, and oem stands for, the OEM code page
func platformLabel(key string) (string, bool) {
	switch key {
	case "mbcs", "ansi", "dbcs":
		return "cp" + strconv.FormatUint(uint64(windows.GetACP()), 10), true
	case "oem":
		if procGetOEMCP.Find() != nil {
			return "", false
		}
		cp, _, _ := procGetOEMCP.Call()
		return "cp" + strconv.FormatUint(uint64(cp), 10), true
	}
	return "", false
}
