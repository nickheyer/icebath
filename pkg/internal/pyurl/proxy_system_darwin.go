//go:build darwin

package pyurl

import (
	"bufio"
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Proxy dictionary and exceptions list scutil prints for the SystemConfiguration proxy settings
func ScutilProxies() (map[string]string, []string) {
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

// Enabled HTTP, HTTPS, FTP and Gopher proxies as http URLs the way getproxies_macosx_sysconf reads them
func SystemProxies() map[string]string {
	values, _ := ScutilProxies()
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
