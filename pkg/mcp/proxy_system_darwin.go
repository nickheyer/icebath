//go:build darwin

package mcp

import (
	"bufio"
	"bytes"
	"context"
	"net/netip"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// scutilProxies parses the SystemConfiguration proxy dictionary that scutil prints
func scutilProxies() (map[string]string, []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/scutil", "--proxy").Output()
	if err != nil {
		return map[string]string{}, nil
	}
	values := map[string]string{}
	var exceptions []string
	inExceptions := false
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if inExceptions {
			if line == "}" {
				inExceptions = false
				continue
			}
			if _, v, ok := strings.Cut(line, " : "); ok {
				exceptions = append(exceptions, v)
			}
			continue
		}
		k, v, ok := strings.Cut(line, " : ")
		if !ok {
			continue
		}
		if k == "ExceptionsList" && strings.HasPrefix(v, "<array>") {
			inExceptions = true
			continue
		}
		values[k] = v
	}
	return values, exceptions
}

// getproxiesSystem returns the enabled HTTP, HTTPS, FTP and Gopher proxies as http URLs like _scproxy
func getproxiesSystem() map[string]string {
	values, _ := scutilProxies()
	proxies := map[string]string{}
	for _, p := range [][2]string{{"http", "HTTP"}, {"https", "HTTPS"}, {"ftp", "FTP"}, {"gopher", "Gopher"}} {
		if n, err := strconv.Atoi(values[p[1]+"Enable"]); err != nil || n == 0 {
			continue
		}
		host, ok := values[p[1]+"Proxy"]
		if !ok {
			continue
		}
		if port, err := strconv.Atoi(values[p[1]+"Port"]); err == nil {
			proxies[p[0]] = "http://" + host + ":" + strconv.Itoa(port)
		} else {
			proxies[p[0]] = "http://" + host
		}
	}
	return proxies
}

var macExceptionIPRE = regexp.MustCompile(`^(\d+(?:\.\d+)*)(/\d+)?`)

// proxyBypassSystem applies the macOS exclude simple hostnames flag and the exceptions list to host
func proxyBypassSystem(host string) bool {
	values, exceptions := scutilProxies()
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
		} else if fnmatchCase(host, value) {
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
