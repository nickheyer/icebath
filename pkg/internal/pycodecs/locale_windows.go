package pycodecs

import (
	"strconv"

	"golang.org/x/sys/windows"
)

var procGetOEMCP = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetOEMCP")

// Code page codec mbcs and oem stand for on Windows, cp65001 always answering
func platformCodePage(mod string) string {
	switch mod {
	case "mbcs":
		return "cp" + strconv.Itoa(int(windows.GetACP()))
	case "oem":
		r, _, _ := procGetOEMCP.Call()
		return "cp" + strconv.Itoa(int(r))
	case "cp65001":
		return "utf_8"
	}
	return ""
}

// locale.getencoding on Windows, the ANSI code page
func LocaleEncoding() string { return "cp" + strconv.Itoa(int(windows.GetACP())) }
