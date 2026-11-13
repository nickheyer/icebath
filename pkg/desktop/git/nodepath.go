package git

import (
	"os"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Posix or win32 flavour of the Node path module the git group reads
type nodePath struct {
	win32 bool
	cwd   func() string
	env   func(string) string
}

var posixPath = nodePath{cwd: processCwd, env: os.Getenv}
var win32Path = nodePath{win32: true, cwd: processCwd, env: os.Getenv}

var hostIsWindows = runtime.GOOS == "windows"

// Node path module of the running platform
var hostPath = func() nodePath {
	if hostIsWindows {
		return win32Path
	}
	return posixPath
}()

func processCwd() string {
	wd, _ := os.Getwd()
	return wd
}

// JavaScript toLowerCase with full Unicode case mapping
func jsLower(s string) string { return pystr.Lower(s) }

// JavaScript toUpperCase with full Unicode case mapping
func jsUpper(s string) string { return pystr.Upper(s) }

// Last path segment with trailing separators ignored, as Node basename without a suffix
func (p nodePath) basename(path string) string {
	isSep, start := isPosixSep, 0
	if p.win32 {
		isSep = isWinSep
		if len(path) >= 2 && isDeviceRootChar(path[0]) && path[1] == ':' {
			start = 2
		}
	}
	end, matchedSlash := -1, true
	for i := len(path) - 1; i >= start; i-- {
		if isSep(path[i]) {
			if !matchedSlash {
				start = i + 1
				break
			}
		} else if end == -1 {
			matchedSlash = false
			end = i + 1
		}
	}
	if end == -1 {
		return ""
	}
	return path[start:end]
}

// Directory part of a path, as Node dirname answers for each flavour
func (p nodePath) dirname(path string) string {
	if !p.win32 {
		if path == "" {
			return "."
		}
		hasRoot := path[0] == '/'
		end, matchedSlash := -1, true
		for i := len(path) - 1; i >= 1; i-- {
			if path[i] == '/' {
				if !matchedSlash {
					end = i
					break
				}
			} else {
				matchedSlash = false
			}
		}
		if end == -1 {
			if hasRoot {
				return "/"
			}
			return "."
		}
		if hasRoot && end == 1 {
			return "//"
		}
		return path[:end]
	}
	n := len(path)
	if n == 0 {
		return "."
	}
	rootEnd, offset := -1, 0
	code := path[0]
	if n == 1 {
		if isWinSep(code) {
			return path
		}
		return "."
	}
	if isWinSep(code) {
		rootEnd, offset = 1, 1
		if isWinSep(path[1]) {
			j := 2
			last := j
			for j < n && !isWinSep(path[j]) {
				j++
			}
			if j < n && j != last {
				last = j
				for j < n && isWinSep(path[j]) {
					j++
				}
				if j < n && j != last {
					last = j
					for j < n && !isWinSep(path[j]) {
						j++
					}
					if j == n {
						return path
					}
					if j != last {
						rootEnd, offset = j+1, j+1
					}
				}
			}
		}
	} else if isDeviceRootChar(code) && path[1] == ':' {
		rootEnd = 2
		if n > 2 && isWinSep(path[2]) {
			rootEnd = 3
		}
		offset = rootEnd
	}
	end, matchedSlash := -1, true
	for i := n - 1; i >= offset; i-- {
		if isWinSep(path[i]) {
			if !matchedSlash {
				end = i
				break
			}
		} else {
			matchedSlash = false
		}
	}
	if end == -1 {
		if rootEnd == -1 {
			return "."
		}
		end = rootEnd
	}
	return path[:end]
}

func (p nodePath) sep() string {
	if p.win32 {
		return `\`
	}
	return "/"
}

func isPosixSep(c byte) bool { return c == '/' }

func isWinSep(c byte) bool { return c == '/' || c == '\\' }

func isDeviceRootChar(c byte) bool { return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') }

var windowsReservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
	"COM¹": true, "COM²": true, "COM³": true, "LPT¹": true, "LPT²": true, "LPT³": true,
}

// True when the text before the colon names a Windows device such as CON or COM1
func isWindowsReservedName(path string, colonIndex int) bool {
	device := path
	if colonIndex >= 0 {
		device = path[:min(colonIndex, len(path))]
	} else {
		r, size := utf8.DecodeLastRuneInString(path)
		if r > 0xFFFF {
			return false
		}
		device = path[:len(path)-size]
	}
	return windowsReservedNames[jsUpper(device)]
}

// Resolves dot segments of a path body with the given separator, as Node normalizeString does
func normalizeString(path string, allowAboveRoot bool, separator string, isSep func(byte) bool) string {
	res := ""
	lastSegmentLength := 0
	lastSlash := -1
	dots := 0
	var code byte
	for i := 0; i <= len(path); i++ {
		if i < len(path) {
			code = path[i]
		} else if isSep(code) {
			break
		} else {
			code = '/'
		}
		if isSep(code) {
			if lastSlash == i-1 || dots == 1 {
			} else if dots == 2 {
				if len(res) < 2 || lastSegmentLength != 2 || res[len(res)-1] != '.' || res[len(res)-2] != '.' {
					if len(res) > 2 {
						idx := strings.LastIndex(res, separator)
						if idx == -1 {
							res = ""
							lastSegmentLength = 0
						} else {
							res = res[:idx]
							lastSegmentLength = len(res) - 1 - strings.LastIndex(res, separator)
						}
						lastSlash = i
						dots = 0
						continue
					} else if len(res) != 0 {
						res = ""
						lastSegmentLength = 0
						lastSlash = i
						dots = 0
						continue
					}
				}
				if allowAboveRoot {
					if len(res) > 0 {
						res += separator + ".."
					} else {
						res = ".."
					}
					lastSegmentLength = 2
				}
			} else {
				if len(res) > 0 {
					res += separator + path[lastSlash+1:i]
				} else {
					res = path[lastSlash+1 : i]
				}
				lastSegmentLength = i - lastSlash - 1
			}
			lastSlash = i
			dots = 0
		} else if code == '.' && dots != -1 {
			dots++
		} else {
			dots = -1
		}
	}
	return res
}

func (p nodePath) isAbsolute(path string) bool {
	if !p.win32 {
		return strings.HasPrefix(path, "/")
	}
	if len(path) == 0 {
		return false
	}
	return isWinSep(path[0]) || (len(path) > 2 && isDeviceRootChar(path[0]) && path[1] == ':' && isWinSep(path[2]))
}

func (p nodePath) normalize(path string) string {
	if !p.win32 {
		return posixNormalize(path)
	}
	return win32Normalize(path)
}

func posixNormalize(path string) string {
	if path == "" {
		return "."
	}
	abs := path[0] == '/'
	trailing := path[len(path)-1] == '/'
	path = normalizeString(path, !abs, "/", isPosixSep)
	if path == "" {
		if abs {
			return "/"
		}
		if trailing {
			return "./"
		}
		return "."
	}
	if trailing {
		path += "/"
	}
	if abs {
		return "/" + path
	}
	return path
}

// Matches the UNC or device root at the start of a win32 path, the shared prefix scan of resolve and normalize
func uncRoot(path string) (firstPart, rest string, last, j int, ok bool) {
	n := len(path)
	j = 2
	last = j
	for j < n && !isWinSep(path[j]) {
		j++
	}
	if !(j < n && j != last) {
		return "", "", 0, 0, false
	}
	firstPart = path[last:j]
	last = j
	for j < n && isWinSep(path[j]) {
		j++
	}
	if !(j < n && j != last) {
		return "", "", 0, 0, false
	}
	last = j
	for j < n && !isWinSep(path[j]) {
		j++
	}
	if j == n || j != last {
		return firstPart, path[last:j], last, j, true
	}
	return "", "", 0, 0, false
}

func win32Normalize(path string) string {
	n := len(path)
	if n == 0 {
		return "."
	}
	rootEnd := 0
	device := ""
	hasDevice := false
	abs := false
	code := path[0]
	if n == 1 {
		if isPosixSep(code) {
			return `\`
		}
		return path
	}
	if isWinSep(code) {
		abs = true
		if isWinSep(path[1]) {
			if firstPart, share, last, j, ok := uncRoot(path); ok {
				if firstPart == "." || firstPart == "?" {
					device, hasDevice = `\\`+firstPart, true
					rootEnd = 4
					colon := strings.Index(path, ":")
					possible := ""
					if 4 <= colon+1 {
						possible = path[4 : colon+1]
					}
					if isWindowsReservedName(possible, len(possible)-1) {
						device = `\\?\` + possible
						rootEnd = 4 + len(possible)
					}
				} else if j == n {
					return `\\` + firstPart + `\` + path[last:] + `\`
				} else {
					device, hasDevice = `\\`+firstPart+`\`+share, true
					rootEnd = j
				}
			}
		} else {
			rootEnd = 1
		}
	} else {
		colon := strings.Index(path, ":")
		if colon > 0 {
			if isDeviceRootChar(code) && colon == 1 {
				device, hasDevice = path[:2], true
				rootEnd = 2
				if n > 2 && isWinSep(path[2]) {
					abs = true
					rootEnd = 3
				}
			} else if isWindowsReservedName(path, colon) {
				device, hasDevice = path[:colon+1], true
				rootEnd = colon + 1
			}
		}
	}
	tail := ""
	if rootEnd < n {
		tail = normalizeString(path[rootEnd:], !abs, `\`, isWinSep)
	}
	if tail == "" && !abs {
		tail = "."
	}
	if tail != "" && isWinSep(path[n-1]) {
		tail += `\`
	}
	if !abs && !hasDevice && strings.Contains(path, ":") {
		if len(tail) >= 2 && isDeviceRootChar(tail[0]) && tail[1] == ':' {
			return `.\` + tail
		}
		for index := strings.Index(path, ":"); index != -1; {
			if index == n-1 || isWinSep(path[index+1]) {
				return `.\` + tail
			}
			next := strings.Index(path[index+1:], ":")
			if next == -1 {
				break
			}
			index += next + 1
		}
	}
	if isWindowsReservedName(path, strings.Index(path, ":")) {
		return `.\` + device + tail
	}
	if !hasDevice {
		if abs {
			return `\` + tail
		}
		return tail
	}
	if abs {
		return device + `\` + tail
	}
	return device + tail
}

func (p nodePath) join(parts ...string) string {
	var kept []string
	for _, a := range parts {
		if a != "" {
			kept = append(kept, a)
		}
	}
	if len(kept) == 0 {
		return "."
	}
	if !p.win32 {
		return posixNormalize(strings.Join(kept, "/"))
	}
	first := kept[0]
	joined := strings.Join(kept, `\`)
	needsReplace := true
	slashCount := 0
	if isWinSep(first[0]) {
		slashCount++
		if len(first) > 1 && isWinSep(first[1]) {
			slashCount++
			if len(first) > 2 {
				if isWinSep(first[2]) {
					slashCount++
				} else {
					needsReplace = false
				}
			}
		}
	}
	if needsReplace {
		for slashCount < len(joined) && isWinSep(joined[slashCount]) {
			slashCount++
		}
		if slashCount >= 2 {
			joined = `\` + joined[slashCount:]
		}
	}
	for _, part := range strings.FieldsFunc(joined, func(r rune) bool { return r == '\\' }) {
		if colon := strings.Index(part, ":"); colon != -1 && isWindowsReservedName(part, colon) {
			return strings.ReplaceAll(joined, "/", `\`)
		}
	}
	return win32Normalize(joined)
}

func (p nodePath) resolve(args ...string) string {
	if !p.win32 {
		return p.posixResolve(args...)
	}
	return p.win32Resolve(args...)
}

func (p nodePath) posixResolve(args ...string) string {
	if len(args) == 0 || (len(args) == 1 && (args[0] == "" || args[0] == ".")) {
		if cwd := p.cwd(); strings.HasPrefix(cwd, "/") {
			return cwd
		}
	}
	resolved := ""
	abs := false
	for i := len(args) - 1; i >= 0 && !abs; i-- {
		if args[i] == "" {
			continue
		}
		resolved = args[i] + "/" + resolved
		abs = args[i][0] == '/'
	}
	if !abs {
		cwd := p.cwd()
		resolved = cwd + "/" + resolved
		abs = strings.HasPrefix(cwd, "/")
	}
	resolved = normalizeString(resolved, !abs, "/", isPosixSep)
	if abs {
		return "/" + resolved
	}
	if resolved != "" {
		return resolved
	}
	return "."
}

func (p nodePath) win32Resolve(args ...string) string {
	resolvedDevice := ""
	resolvedTail := ""
	resolvedAbs := false
	for i := len(args) - 1; i >= -1; i-- {
		var path string
		if i >= 0 {
			path = args[i]
			if path == "" {
				continue
			}
		} else if resolvedDevice == "" {
			path = p.cwd()
			if len(args) == 0 || (len(args) == 1 && (args[0] == "" || args[0] == ".") && path != "" && isWinSep(path[0])) {
				if !hostIsWindows {
					path = strings.ReplaceAll(path, "/", `\`)
				}
				return path
			}
		} else {
			path = p.env("=" + resolvedDevice)
			if path == "" {
				path = p.cwd()
			}
			if len(path) > 2 && jsLower(path[:2]) != jsLower(resolvedDevice) && path[2] == '\\' {
				path = resolvedDevice + `\`
			}
		}
		n := len(path)
		rootEnd := 0
		device := ""
		abs := false
		code := byte(0)
		if n > 0 {
			code = path[0]
		}
		if n == 1 {
			if isWinSep(code) {
				rootEnd = 1
				abs = true
			}
		} else if n > 0 && isWinSep(code) {
			abs = true
			if isWinSep(path[1]) {
				if firstPart, share, _, j, ok := uncRoot(path); ok {
					if firstPart != "." && firstPart != "?" {
						device = `\\` + firstPart + `\` + share
						rootEnd = j
					} else {
						device = `\\` + firstPart
						rootEnd = 4
					}
				}
			} else {
				rootEnd = 1
			}
		} else if n > 1 && isDeviceRootChar(code) && path[1] == ':' {
			device = path[:2]
			rootEnd = 2
			if n > 2 && isWinSep(path[2]) {
				abs = true
				rootEnd = 3
			}
		}
		if device != "" {
			if resolvedDevice != "" {
				if jsLower(device) != jsLower(resolvedDevice) {
					continue
				}
			} else {
				resolvedDevice = device
			}
		}
		if resolvedAbs {
			if resolvedDevice != "" {
				break
			}
		} else {
			resolvedTail = path[rootEnd:] + `\` + resolvedTail
			resolvedAbs = abs
			if abs && resolvedDevice != "" {
				break
			}
		}
	}
	resolvedTail = normalizeString(resolvedTail, !resolvedAbs, `\`, isWinSep)
	if resolvedAbs {
		return resolvedDevice + `\` + resolvedTail
	}
	if out := resolvedDevice + resolvedTail; out != "" {
		return out
	}
	return "."
}

func (p nodePath) relative(from, to string) string {
	if !p.win32 {
		return p.posixRelative(from, to)
	}
	return p.win32Relative(from, to)
}

func (p nodePath) posixRelative(from, to string) string {
	if from == to {
		return ""
	}
	from = p.posixResolve(from)
	to = p.posixResolve(to)
	if from == to {
		return ""
	}
	fromStart, fromEnd := 1, len(from)
	fromLen := fromEnd - fromStart
	toStart := 1
	toLen := len(to) - toStart
	length := min(fromLen, toLen)
	lastCommonSep := -1
	i := 0
	for ; i < length; i++ {
		c := from[fromStart+i]
		if c != to[toStart+i] {
			break
		} else if c == '/' {
			lastCommonSep = i
		}
	}
	if i == length {
		if toLen > length {
			if to[toStart+i] == '/' {
				return to[toStart+i+1:]
			}
			if i == 0 {
				return to[toStart+i:]
			}
		} else if fromLen > length {
			if from[fromStart+i] == '/' {
				lastCommonSep = i
			} else if i == 0 {
				lastCommonSep = 0
			}
		}
	}
	out := ""
	for i = fromStart + lastCommonSep + 1; i <= fromEnd; i++ {
		if i == fromEnd || from[i] == '/' {
			if out == "" {
				out = ".."
			} else {
				out += "/.."
			}
		}
	}
	return out + to[toStart+lastCommonSep:]
}

func (p nodePath) win32Relative(from, to string) string {
	if from == to {
		return ""
	}
	fromOrig := p.win32Resolve(from)
	toOrig := p.win32Resolve(to)
	if fromOrig == toOrig {
		return ""
	}
	from = jsLower(fromOrig)
	to = jsLower(toOrig)
	if from == to {
		return ""
	}
	if len(fromOrig) != len(from) || len(toOrig) != len(to) {
		fromSplit := strings.Split(fromOrig, `\`)
		toSplit := strings.Split(toOrig, `\`)
		if fromSplit[len(fromSplit)-1] == "" {
			fromSplit = fromSplit[:len(fromSplit)-1]
		}
		if toSplit[len(toSplit)-1] == "" {
			toSplit = toSplit[:len(toSplit)-1]
		}
		fromLen, toLen := len(fromSplit), len(toSplit)
		length := min(fromLen, toLen)
		i := 0
		for ; i < length; i++ {
			if jsLower(fromSplit[i]) != jsLower(toSplit[i]) {
				break
			}
		}
		if i == 0 {
			return toOrig
		} else if i == length {
			if toLen > length {
				return strings.Join(toSplit[i:], `\`)
			}
			if fromLen > length {
				return strings.Repeat(`..\`, fromLen-1-i) + ".."
			}
			return ""
		}
		return strings.Repeat(`..\`, fromLen-i) + strings.Join(toSplit[i:], `\`)
	}
	fromStart := 0
	for fromStart < len(from) && from[fromStart] == '\\' {
		fromStart++
	}
	fromEnd := len(from)
	for fromEnd-1 > fromStart && from[fromEnd-1] == '\\' {
		fromEnd--
	}
	fromLen := fromEnd - fromStart
	toStart := 0
	for toStart < len(to) && to[toStart] == '\\' {
		toStart++
	}
	toEnd := len(to)
	for toEnd-1 > toStart && to[toEnd-1] == '\\' {
		toEnd--
	}
	toLen := toEnd - toStart
	length := min(fromLen, toLen)
	lastCommonSep := -1
	i := 0
	for ; i < length; i++ {
		c := from[fromStart+i]
		if c != to[toStart+i] {
			break
		} else if c == '\\' {
			lastCommonSep = i
		}
	}
	if i != length {
		if lastCommonSep == -1 {
			return toOrig
		}
	} else {
		if toLen > length {
			if to[toStart+i] == '\\' {
				return toOrig[toStart+i+1:]
			}
			if i == 2 {
				return toOrig[toStart+i:]
			}
		}
		if fromLen > length {
			if from[fromStart+i] == '\\' {
				lastCommonSep = i
			} else if i == 2 {
				lastCommonSep = 3
			}
		}
		if lastCommonSep == -1 {
			lastCommonSep = 0
		}
	}
	out := ""
	for i = fromStart + lastCommonSep + 1; i <= fromEnd; i++ {
		if i == fromEnd || from[i] == '\\' {
			if out == "" {
				out = ".."
			} else {
				out += `\..`
			}
		}
	}
	toStart += lastCommonSep
	if out != "" {
		return out + toOrig[toStart:toEnd]
	}
	if toStart < len(toOrig) && toOrig[toStart] == '\\' {
		toStart++
	}
	return toOrig[toStart:toEnd]
}
