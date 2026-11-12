package tui

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

const (
	jsonRPCMethodNotFound  = -32601
	jsonRPCSessionNotShown = 4404
	maxBreadcrumbUnits     = 4096
)

var (
	userInfoFallback = regexp.MustCompile(`(?i)^([a-z][a-z0-9+.-]*://)[^/?#@]*@`)
	lineBreaks       = regexp.MustCompile(`[\r\n]+`)
	defaultPorts     = map[string]string{"ws": "80", "wss": "443", "http": "80", "https": "443", "ftp": "21"}
	breadcrumbWarned sync.Once
)

// URL parts as a WHATWG parser yields them for a special scheme
type whatwgURL struct {
	scheme, userinfo, host, path, query string
}

// Parses a URL the way the WHATWG URL constructor accepts it, ok false where it throws
func parseWHATWG(raw string) (whatwgURL, bool) {
	u, err := url.Parse(strings.TrimFunc(raw, isJSSpace))
	if err != nil || u.Scheme == "" || u.Opaque != "" {
		return whatwgURL{}, false
	}
	out := whatwgURL{scheme: strings.ToLower(u.Scheme), query: u.RawQuery}
	if u.User != nil {
		out.userinfo = u.User.String()
	}
	_, special := defaultPorts[out.scheme]
	host := strings.ToLower(u.Hostname())
	if special && host == "" {
		return whatwgURL{}, false
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n > 65535 {
			return whatwgURL{}, false
		}
		if defaultPorts[out.scheme] != strconv.Itoa(n) {
			host += ":" + strconv.Itoa(n)
		}
	}
	out.host = host
	out.path = u.EscapedPath()
	if out.path == "" && special {
		out.path = "/"
	}
	return out, true
}

// URL safe for a log line, user info and query string replaced, even on URLs the parser rejects
func redactURL(raw string) string {
	if raw == "" {
		return raw
	}
	u, ok := parseWHATWG(raw)
	if !ok {
		noUserInfo := userInfoFallback.ReplaceAllString(raw, "${1}***@")
		if i := strings.Index(noUserInfo, "?"); i >= 0 {
			return noUserInfo[:i] + "?***"
		}
		return noUserInfo
	}
	userInfo, query := "", ""
	if u.userinfo != "" {
		userInfo = "***@"
	}
	if u.query != "" {
		query = "?***"
	}
	return u.scheme + "://" + userInfo + u.host + u.path + query
}

// Appends a lifecycle breadcrumb to the TUI gateway crash log so the reason survives the TUI exiting
func recordParentLifecycle(line string) {
	oneLine := lineBreaks.ReplaceAllString(line, " ↵ ")
	if n := unitLen(oneLine); n > maxBreadcrumbUnits {
		oneLine = fmt.Sprintf("%s… [truncated %d chars]", sliceUnits(oneLine, maxBreadcrumbUnits), n)
	}
	logDir := filepath.Join(home.Dir(context.Background()), "logs")
	err := os.MkdirAll(logDir, 0o777)
	if err == nil {
		var f *os.File
		if f, err = os.OpenFile(filepath.Join(logDir, "tui_gateway_crash.log"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666); err == nil {
			_, err = fmt.Fprintf(f, "[tui-parent] %s %s\n", time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), oneLine)
			if closeErr := f.Close(); err == nil {
				err = closeErr
			}
		}
	}
	if err != nil {
		breadcrumbWarned.Do(func() { fmt.Fprint(os.Stderr, "icebath-tui: parent lifecycle log unavailable\n") })
	}
}
