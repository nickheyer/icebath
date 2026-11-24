//go:build darwin

package httputil

import (
	"context"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Settings scutil --proxy prints for the SystemConfiguration store: the HTTP and HTTPS proxies, a SOCKS proxy serving the schemes they leave unset, the exceptions list and the simple host name flag
func loadSystemSettings() *systemSettings {
	values, exceptions := scutilProxySettings()
	s := &systemSettings{proxies: map[string]string{}, exceptions: exceptions}
	if proxy := scutilProxy(values, "HTTP", "http"); proxy != "" {
		s.proxies["http"] = proxy
	}
	if proxy := scutilProxy(values, "HTTPS", "http"); proxy != "" {
		s.proxies["https"] = proxy
	}
	if socks := scutilProxy(values, "SOCKS", "socks5"); socks != "" {
		for _, scheme := range [...]string{"http", "https"} {
			if s.proxies[scheme] == "" {
				s.proxies[scheme] = socks
			}
		}
	}
	n, _ := strconv.Atoi(values["ExcludeSimpleHostnames"])
	s.bypassSimple = n != 0
	return s
}

// Top level values and the ExceptionsList items scutil --proxy prints, nested dictionaries skipped, empty when scutil cannot run
func scutilProxySettings() (map[string]string, []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/scutil", "--proxy").Output()
	if err != nil {
		return map[string]string{}, nil
	}
	values := map[string]string{}
	var exceptions []string
	depth, inExceptions := 0, false
	for line := range strings.Lines(string(out)) {
		line = strings.TrimSpace(line)
		switch {
		case line == "}":
			depth--
			if depth <= 1 {
				inExceptions = false
			}
			continue
		case strings.HasSuffix(line, "{"):
			if key, _, _ := strings.Cut(line, " : "); depth == 1 && key == "ExceptionsList" {
				inExceptions = true
			}
			depth++
			continue
		}
		key, value, ok := strings.Cut(line, " : ")
		if !ok {
			continue
		}
		switch {
		case inExceptions && depth == 2:
			exceptions = append(exceptions, value)
		case depth == 1:
			values[key] = value
		}
	}
	return values, exceptions
}

// Proxy URL of one scutil proxy kind, empty when it is disabled or names no host
func scutilProxy(values map[string]string, kind, scheme string) string {
	if n, err := strconv.Atoi(values[kind+"Enable"]); err != nil || n == 0 {
		return ""
	}
	host := values[kind+"Proxy"]
	if host == "" {
		return ""
	}
	if port, err := strconv.Atoi(values[kind+"Port"]); err == nil && port > 0 && port < 65536 {
		return scheme + "://" + net.JoinHostPort(host, strconv.Itoa(port))
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host
}
