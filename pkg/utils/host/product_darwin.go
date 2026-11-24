package hostutil

import (
	"os"
	"runtime"

	"golang.org/x/sys/unix"

	plistutil "github.com/nickheyer/icebath/pkg/utils/plist"
)

// Product name and version of the Apple system, from the kernel first and the system version property list second, empty when neither says
func productRelease() (string, string) {
	product := "macOS"
	if runtime.GOOS == "ios" {
		product = "iOS"
	}
	if version, err := unix.Sysctl("kern.osproductversion"); err == nil && version != "" {
		return product, version
	}
	raw, err := os.ReadFile("/System/Library/CoreServices/SystemVersion.plist")
	if err != nil {
		return "", ""
	}
	doc, err := plistutil.Decode(raw)
	if err != nil {
		return "", ""
	}
	dict, _ := doc.(map[string]any)
	if version, _ := dict["ProductVersion"].(string); version != "" {
		return product, version
	}
	return "", ""
}
