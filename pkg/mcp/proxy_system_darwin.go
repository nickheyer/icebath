//go:build darwin

package mcp

import (
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/fnmatch"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

var macExceptionIPRE = regexp.MustCompile(`^(\d+(?:\.\d+)*)(/\d+)?`)

// proxyBypassSystem applies the macOS exclude simple hostnames flag and the exceptions list to host
func proxyBypassSystem(host string) bool {
	values, exceptions := pyurl.ScutilProxies()
	n, _ := strconv.Atoi(values["ExcludeSimpleHostnames"])
	return macProxyBypass(host, n != 0, exceptions)
}

// macProxyBypass matches host against exceptions as IPv4 prefixes or glob patterns like urllib
func macProxyBypass(host string, excludeSimple bool, exceptions []string) bool {
	hostonly, _ := pySplitPort(host)
	if !strings.Contains(host, ".") && excludeSimple {
		return true
	}
	var hostIP uint32
	haveIP := false
	if addr, err := netip.ParseAddr(hostonly); err == nil && addr.Is4() {
		b := addr.As4()
		hostIP, haveIP = uint32(b[0])<<24|uint32(b[1])<<16|uint32(b[2])<<8|uint32(b[3]), true
	}
	for _, value := range exceptions {
		if value == "" {
			continue
		}
		m := macExceptionIPRE.FindStringSubmatch(value)
		if m != nil && haveIP {
			base, ok := macIP2Num(m[1])
			if !ok {
				continue
			}
			mask := 8 * (strings.Count(m[1], ".") + 1)
			if m[2] != "" {
				mask, _ = strconv.Atoi(m[2][1:])
			}
			if mask < 0 || mask > 32 {
				continue
			}
			shift := uint(32 - mask)
			if uint64(hostIP)>>shift == base>>shift {
				return true
			}
		} else if fnmatch.FnmatchCase(host, value) {
			return true
		}
	}
	return false
}

// macIP2Num packs up to four dotted parts into a number, false when a part alone already exceeds 32 bits
func macIP2Num(ip string) (uint64, bool) {
	parts := strings.Split(ip, ".")
	nums := make([]uint64, 0, 4)
	for _, p := range parts {
		v, err := strconv.ParseUint(p, 10, 64)
		if err != nil || v >= 1<<32 {
			return 0, false
		}
		nums = append(nums, v)
	}
	if len(nums) != 4 {
		nums = append(nums, 0, 0, 0, 0)[:4]
	}
	return nums[0]<<24 | nums[1]<<16 | nums[2]<<8 | nums[3], true
}
