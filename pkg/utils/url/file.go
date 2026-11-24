package urlutil

import (
	"context"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Error for a URL that names no file on this machine: another scheme, or a file URL naming another host
type FileURLError struct {
	URL, Reason string
}

// Reason followed by the quoted URL
func (e *FileURLError) Error() string { return e.Reason + ": " + strconv.Quote(e.URL) }

// file URL of a local path made absolute first, file:///C:/... for a Windows drive path and file://server/share/... for a UNC path
func FileURL(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if runtime.GOOS == "windows" {
		return windowsFileURL(path)
	}
	return "file://" + Escape(path, "/")
}

// file URL of an absolute Windows path, the \\?\ and \\?\UNC\ long path prefixes dropped
func windowsFileURL(path string) string {
	path = strings.ReplaceAll(path, `\`, "/")
	if rest, ok := strings.CutPrefix(path, "//?/"); ok {
		if len(rest) >= 4 && strings.EqualFold(rest[:4], "UNC/") {
			path = "//" + rest[4:]
		} else {
			path = rest
		}
	}
	if len(path) >= 2 && path[1] == ':' && ('a' <= path[0] && path[0] <= 'z' || 'A' <= path[0] && path[0] <= 'Z') {
		return "file:///" + path[:2] + Escape(path[2:], "/")
	}
	return "file:" + Escape(path, "/")
}

// Local path a file URL names, percent escapes decoded; an empty, localhost or own host name authority is local, another one is a UNC share on Windows and a *FileURLError elsewhere, as is a scheme other than file
func FilePath(fileURL string) (string, error) {
	return filePath(context.Background(), fileURL, false)
}

// Local path a file URL names like FilePath, an authority that resolves to a loopback address or one of this machine's interface addresses also counting as local
func LookupFilePath(ctx context.Context, fileURL string) (string, error) {
	return filePath(ctx, fileURL, true)
}

func filePath(ctx context.Context, fileURL string, lookup bool) (string, error) {
	p, err := Split(fileURL)
	if err != nil {
		return "", err
	}
	if p.Scheme != "file" {
		return "", &FileURLError{URL: fileURL, Reason: "not a file url"}
	}
	if runtime.GOOS == "windows" {
		return windowsFilePath(ctx, p.Authority, p.Path, lookup)
	}
	local, err := localAuthority(ctx, p.Authority, lookup)
	if err != nil {
		return "", err
	}
	if !local {
		return "", &FileURLError{URL: fileURL, Reason: "file url names another host"}
	}
	return Unescape(p.Path), nil
}

// Windows path of a file URL authority and path: a drive letter in either, a remote host as a UNC share, | read as the drive colon
func windowsFilePath(ctx context.Context, authority, path string, lookup bool) (string, error) {
	if len(authority) >= 2 && authority[1] == ':' {
		path = authority + path
	} else {
		local, err := localAuthority(ctx, authority, lookup)
		if err != nil {
			return "", err
		}
		switch {
		case !local:
			path = "//" + authority + path
		case strings.HasPrefix(path, "///"):
			path = path[1:]
		default:
			if len(path) >= 3 && path[0] == '/' && (path[2] == ':' || path[2] == '|') {
				path = path[1:]
			}
			if len(path) >= 2 && path[1] == '|' {
				path = path[:1] + ":" + path[2:]
			}
		}
	}
	return Unescape(strings.ReplaceAll(path, "/", `\`)), nil
}

// Whether a file URL authority is this machine: empty, localhost or the host name, and with lookup one resolving to a loopback or interface address
func localAuthority(ctx context.Context, authority string, lookup bool) (bool, error) {
	if authority == "" || strings.EqualFold(authority, "localhost") {
		return true, nil
	}
	if name, err := os.Hostname(); err == nil && strings.EqualFold(authority, name) {
		return true, nil
	}
	if !lookup {
		return false, nil
	}
	resolved, err := net.DefaultResolver.LookupNetIP(ctx, "ip", authority)
	if err != nil {
		return false, ctx.Err()
	}
	own, err := net.InterfaceAddrs()
	if err != nil {
		return false, err
	}
	for _, addr := range resolved {
		addr = addr.Unmap().WithZone("")
		if addr.IsLoopback() {
			return true, nil
		}
		for _, a := range own {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if mine, ok := netip.AddrFromSlice(ip); ok && mine.Unmap() == addr {
				return true, nil
			}
		}
	}
	return false, nil
}
