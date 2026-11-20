package pyjsonschema

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/netpref"
	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// Frames _warn_for_remote_retrieve reaches and the redirects the urlopen path is nested in
type fetchFrames struct{ depth, redirects int }

// Raises the deepest frame count to a chain n frames below urlopen, five more per enclosing redirect
func (f *fetchFrames) note(n int) { f.depth = max(f.depth, n+5*f.redirects) }

// Remote retrieval of the jsonschema default registry, urlopen of the URI parsed as a draft 2020-12 resource
func retrieveRemote(uri string, st *state) (res *resource) {
	f := &fetchFrames{depth: 5}
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(*Exception); ok && e.Type != "NoSuchResource" && e.Type != "CannotDetermineSpecification" {
				st.touch(f.depth)
				raise("Unretrievable", repr(uri))
			}
			panic(r)
		}
	}()
	body := urlopen(uri, map[string]int{}, f)
	st.touch(f.depth)
	parsed, err := pyjson.LoadsBytes(body)
	if err != nil {
		raise("JSONDecodeError", err.Error())
	}
	contents := pyValue(parsed)
	return spec202012.detect(contents).createResource(contents)
}

const retrieveUserAgent = "python-jsonschema (deprecated $ref resolution)"

var (
	typeRe = regexp.MustCompile(`(?s)^([^/:]+):(.*)$`)
	hostRe = regexp.MustCompile(`(?s)^//([^/#?]*)(.*)$`)
	portRe = regexp.MustCompile(`(?s)^(.*):([0-9]*)$`)
	pasvRe = regexp.MustCompile(`([0-9]+),([0-9]+),([0-9]+),([0-9]+),([0-9]+),([0-9]+)`)
)

func unwrapURL(u string) string {
	u = pystr.Strip(u)
	if strings.HasPrefix(u, "<") && strings.HasSuffix(u, ">") && len(u) >= 2 {
		u = pystr.Strip(u[1 : len(u)-1])
	}
	if strings.HasPrefix(u, "URL:") {
		u = pystr.Strip(u[4:])
	}
	return u
}

// _splithost, the authority after a double slash and the path made absolute
func splitHostPath(rest string) (string, string, bool) {
	m := hostRe.FindStringSubmatch(rest)
	if m == nil {
		return "", rest, false
	}
	path := m[2]
	if path != "" && path[0] != '/' {
		path = "/" + path
	}
	return m[1], path, true
}

func splitPort(host string) (string, string) {
	if m := portRe.FindStringSubmatch(host); m != nil && m[2] != "" {
		return m[1], m[2]
	}
	return host, ""
}

// Proxy a request was routed through, the URL scheme the proxy speaks and its basic credentials
type proxyRoute struct {
	scheme, hostport string
	auth             *url.Userinfo
}

// Body bytes the default urllib opener handlers read for a URL
func urlopen(fullURL string, visited map[string]int, f *fetchFrames) []byte {
	full := unwrapURL(fullURL)
	if i := strings.LastIndexByte(full, '#'); i >= 0 {
		full = full[:i]
	}
	m := typeRe.FindStringSubmatch(full)
	if m == nil {
		raise("ValueError", "unknown url type: "+repr(full))
	}
	typ, rest := strings.ToLower(m[1]), m[2]
	f.note(7)
	host, selector, hasHost := splitHostPath(rest)
	if hasHost {
		host = pyurl.Unquote(host)
	}
	proxies := pyurl.Getproxies()
	if proxy, ok := proxies[typ]; ok && !(host != "" && pyurl.ProxyBypassEnvironment(host, proxies)) {
		proxyType, hostport, auth := parseProxy(proxy)
		if proxyType == "" {
			proxyType = typ
		}
		route := &proxyRoute{scheme: proxyType, hostport: pyurl.Unquote(hostport), auth: auth}
		if typ == "https" {
			route.scheme = "http"
			return httpOpen(full, route, visited, f)
		}
		return openTyped(proxyType, route.hostport, full, full, route, visited, f)
	}
	return openTyped(typ, host, selector, full, nil, visited, f)
}

// _parse_proxy, the proxy type, host and port and the credentials when both user and password are set
func parseProxy(proxy string) (string, string, *url.Userinfo) {
	scheme, authority := "", proxy
	if m := typeRe.FindStringSubmatch(proxy); m != nil && strings.HasPrefix(m[2], "/") {
		if !strings.HasPrefix(m[2], "//") {
			raise("ValueError", "proxy URL with no authority: "+repr(proxy))
		}
		scheme = strings.ToLower(m[1])
		authority = m[2][2:]
		if end := strings.IndexByte(authority, '/'); end >= 0 {
			authority = authority[:end]
		}
	}
	userinfo, hostport := "", authority
	if i := strings.LastIndexByte(authority, '@'); i >= 0 {
		userinfo, hostport = authority[:i], authority[i+1:]
	}
	user, password, hasPassword := strings.Cut(userinfo, ":")
	if user != "" && hasPassword && password != "" {
		return scheme, hostport, url.UserPassword(pyurl.Unquote(user), pyurl.Unquote(password))
	}
	return scheme, hostport, nil
}

// Handler dispatch of OpenerDirector for the request type, a URL routed through a proxy keeping its full form
func openTyped(typ, host, selector, full string, proxy *proxyRoute, visited map[string]int, f *fetchFrames) []byte {
	switch typ {
	case "http", "https":
		return httpOpen(full, proxy, visited, f)
	case "file":
		return fileOpen(full, f)
	case "data":
		return dataOpen(full, f)
	case "ftp":
		return ftpOpen(host, selector, f)
	}
	raise("URLError", "<urlopen error unknown url type: "+typ+">")
	return nil
}

// HTTP fetch with urllib headers, redirects followed and other non 2xx statuses raised as HTTPError
func httpOpen(full string, proxy *proxyRoute, visited map[string]int, f *fetchFrames) []byte {
	target, err := url.Parse(full)
	if err != nil {
		raise("URLError", "<urlopen error "+err.Error()+">")
	}
	var resp *http.Response
	if target.Scheme == "http" || target.Scheme == "https" {
		if target.Host == "" {
			raise("URLError", "<urlopen error no host given>")
		}
		transport := &http.Transport{DialContext: netpref.DialContext((&net.Dialer{}).DialContext), DisableCompression: true, TLSClientConfig: &tls.Config{}}
		if proxy != nil {
			proxyURL := &url.URL{Scheme: proxy.scheme, Host: proxy.hostport, User: proxy.auth}
			transport.Proxy = func(*http.Request) (*url.URL, error) { return proxyURL, nil }
		}
		client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		req, err := http.NewRequest(http.MethodGet, full, nil)
		if err != nil {
			raise("URLError", "<urlopen error "+err.Error()+">")
		}
		req.Header.Set("User-Agent", retrieveUserAgent)
		req.Header.Set("Accept-Encoding", "identity")
		req.Close = true
		resp, err = client.Do(req)
		if err != nil {
			f.note(18)
			raise("URLError", "<urlopen error "+err.Error()+">")
		}
	} else {
		resp = proxiedForeignGet(full, target, proxy, visited, f)
	}
	headerFrames := 19
	if resp.Header.Get("Content-Type") != "" {
		headerFrames = 22
	}
	f.note(headerFrames)
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if readErr != nil {
			raise("IncompleteRead", readErr.Error())
		}
		return body
	}
	switch resp.StatusCode {
	case 301, 302, 303, 307, 308:
		location := resp.Header.Get("Location")
		if location == "" {
			location = resp.Header.Get("Uri")
		}
		if location != "" {
			return followRedirect(full, location, resp.StatusCode, visited, f)
		}
	}
	raise("HTTPError", fmt.Sprintf("HTTP Error %d: %s", resp.StatusCode, strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)+" ")))
	return nil
}

// Absolute form request for an ftp, file or data URL sent through an HTTP proxy as http.client sends it
func proxiedForeignGet(full string, target *url.URL, proxy *proxyRoute, visited map[string]int, f *fetchFrames) *http.Response {
	if proxy == nil {
		raise("URLError", "<urlopen error unknown url type: "+target.Scheme+">")
	}
	host, port := splitPort(proxy.hostport)
	if port == "" {
		port = "80"
		if proxy.scheme == "https" {
			port = "443"
		}
	}
	conn, err := net.Dial("tcp", net.JoinHostPort(strings.Trim(host, "[]"), port))
	if err != nil {
		f.note(18)
		raise("URLError", "<urlopen error "+err.Error()+">")
	}
	if proxy.scheme == "https" {
		tc := tls.Client(conn, &tls.Config{ServerName: strings.Trim(host, "[]")})
		if err := tc.Handshake(); err != nil {
			conn.Close()
			raise("URLError", "<urlopen error "+err.Error()+">")
		}
		conn = tc
	}
	var b strings.Builder
	b.WriteString("GET " + full + " HTTP/1.1\r\n")
	b.WriteString("Host: " + target.Host + "\r\n")
	b.WriteString("Accept-Encoding: identity\r\n")
	b.WriteString("User-Agent: " + retrieveUserAgent + "\r\n")
	if proxy.auth != nil {
		p, _ := proxy.auth.Password()
		b.WriteString("Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(proxy.auth.Username()+":"+p)) + "\r\n")
	}
	b.WriteString("Connection: close\r\n\r\n")
	if _, err := io.WriteString(conn, b.String()); err != nil {
		conn.Close()
		raise("URLError", "<urlopen error "+err.Error()+">")
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		conn.Close()
		raise("RemoteDisconnected", err.Error())
	}
	resp.Body = struct {
		io.Reader
		io.Closer
	}{resp.Body, conn}
	return resp
}

// HTTPRedirectHandler, the new URL quoted and joined with loop and length limits before reopening
func followRedirect(full, location string, code int, visited map[string]int, f *fetchFrames) []byte {
	parts, err := pyurl.Parse(location)
	if err != nil {
		raise("ValueError", err.Error())
	}
	if parts.Path == "" && parts.Netloc != "" {
		parts.Path = "/"
		location = pyurl.Unparse(parts)
	}
	location = quoteLatin1Punctuation(location)
	newURL := urljoin(full, location)
	split, err := pyurl.Split(newURL)
	if err != nil {
		raise("ValueError", err.Error())
	}
	switch split.Scheme {
	case "http", "https", "ftp", "":
	default:
		raise("HTTPError", fmt.Sprintf("HTTP Error %d: redirection to url '%s' is not allowed", code, newURL))
	}
	if visited[newURL] >= 4 || len(visited) >= 10 {
		raise("HTTPError", fmt.Sprintf("HTTP Error %d: The HTTP server returned a redirect error that would lead to an infinite loop.", code))
	}
	visited[newURL]++
	f.redirects++
	defer func() { f.redirects-- }()
	return urlopen(newURL, visited, f)
}

// quote with string.punctuation safe under iso-8859-1, the encoding the redirect handler applies
func quoteLatin1Punctuation(s string) string {
	const punctuation = "!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~"
	var b strings.Builder
	for _, r := range pystr.Runes(s) {
		switch {
		case r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(punctuation, r)):
			b.WriteRune(r)
		case r < 0x100:
			fmt.Fprintf(&b, "%%%02X", r)
		default:
			raise("UnicodeEncodeError", fmt.Sprintf("'latin-1' codec can't encode character '\\u%04x'", r))
		}
	}
	return b.String()
}

// FileHandler, the local file a file URL names
func fileOpen(full string, f *fetchFrames) []byte {
	path, err := pyurl.URL2Pathname(full, true, true)
	if err != nil {
		raise("URLError", err.Error())
	}
	body, err := os.ReadFile(path)
	if err != nil {
		f.note(8)
		raise("URLError", "<urlopen error "+err.Error()+">")
	}
	f.note(18)
	return body
}

// DataHandler, the bytes of an RFC 2397 data URL
func dataOpen(full string, f *fetchFrames) []byte {
	_, data, _ := strings.Cut(full, ":")
	mediatype, payload, ok := strings.Cut(data, ",")
	if !ok {
		raise("ValueError", "not enough values to unpack (expected 2, got 1)")
	}
	for i := 0; i < len(mediatype); i++ {
		if mediatype[i] < 0x20 || mediatype[i] == 0x7f {
			raise("ValueError", "Control characters not allowed in data: mediatype")
		}
	}
	raw := unquoteToBytes(payload)
	if strings.HasSuffix(mediatype, ";base64") {
		ascii := make([]byte, 0, len(raw))
		for _, c := range raw {
			if c < 0x80 {
				ascii = append(ascii, c)
			}
		}
		decoded, err := pybase64.B64Decode(string(ascii))
		if err != nil {
			raise("Error", err.Error())
		}
		raw = decoded
	}
	f.note(18)
	return raw
}

// urllib.parse.unquote_to_bytes of a str, percent escapes decoded over its UTF-8 bytes
func unquoteToBytes(s string) []byte {
	in := []byte(s)
	out := make([]byte, 0, len(in))
	for i := 0; i < len(in); i++ {
		if in[i] == '%' && i+2 < len(in) && isHexDigit(in[i+1]) && isHexDigit(in[i+2]) {
			v, _ := strconv.ParseUint(string(in[i+1:i+3]), 16, 8)
			out = append(out, byte(v))
			i += 2
			continue
		}
		out = append(out, in[i])
	}
	return out
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// FTPHandler over ftplib, a binary RETR or a LIST over a passive data connection
func ftpOpen(host, selector string, f *fetchFrames) []byte {
	if host == "" {
		raise("URLError", "<urlopen error ftp error: no host given>")
	}
	hostOnly, port := splitPort(host)
	portNum := 21
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil {
			raise("ValueError", "invalid literal for int() with base 10: "+repr(port))
		}
		portNum = n
	}
	user, passwd := "", ""
	if i := strings.LastIndexByte(hostOnly, '@'); i >= 0 {
		user, passwd, _ = strings.Cut(hostOnly[:i], ":")
		hostOnly = hostOnly[i+1:]
	}
	hostOnly = pyurl.Unquote(hostOnly)
	addrs, err := net.DefaultResolver.LookupIP(context.Background(), "ip4", hostOnly)
	if err != nil || len(addrs) == 0 {
		raise("URLError", fmt.Sprintf("<urlopen error %v>", err))
	}
	words := strings.Split(selector, ";")
	dirs := strings.Split(words[0], "/")
	for i := range dirs {
		dirs[i] = pyurl.Unquote(dirs[i])
	}
	file := dirs[len(dirs)-1]
	dirs = dirs[:len(dirs)-1]
	if len(dirs) > 0 && dirs[0] == "" {
		dirs = dirs[1:]
	}
	typ := "D"
	if file != "" {
		typ = "I"
	}
	for _, attr := range words[1:] {
		name, value, hasValue := strings.Cut(attr, "=")
		if hasValue && strings.ToLower(name) == "type" && strings.Contains("aAiIdD", value) && len(value) == 1 {
			typ = strings.ToUpper(value)
		}
	}
	w := &ftpWrapper{user: user, passwd: passwd, host: addrs[0].String(), port: portNum, dirs: dirs}
	defer func() {
		if w.f != nil {
			w.f.conn.Close()
		}
	}()
	f.note(15)
	w.init()
	f.note(17)
	data := w.retrfile(file, typ)
	defer data.Close()
	body, err := io.ReadAll(data)
	if err != nil {
		raise("URLError", "<urlopen error ftp error: "+err.Error()+">")
	}
	f.note(18)
	return body
}

// urllib ftpwrapper, the connection it logs in and changes into the URL directories with
type ftpWrapper struct {
	user, passwd, host string
	port               int
	dirs               []string
	f                  *ftpClient
}

func (w *ftpWrapper) init() {
	if w.f != nil {
		w.f.conn.Close()
	}
	w.f = ftpConnect(w.host, w.port)
	w.f.login(w.user, w.passwd)
	w.f.cwd(strings.Join(w.dirs, "/"))
}

// retrfile, RETR for a file falling back to a LIST when the server answers 550
func (w *ftpWrapper) retrfile(file, typ string) net.Conn {
	cmd, isdir := "TYPE "+typ, false
	if typ == "d" || typ == "D" {
		cmd, isdir = "TYPE A", true
	}
	if ftpFailed(func() { w.f.voidcmd(cmd) }) != nil {
		w.init()
		w.f.voidcmd(cmd)
	}
	var conn net.Conn
	if file != "" && !isdir {
		if err := ftpFailed(func() { conn = w.f.transfer("RETR " + file) }); err != nil {
			if err.Type != "error_perm" || !strings.HasPrefix(err.Msg, "550") {
				raise("URLError", "<urlopen error ftp error: "+err.Msg+">")
			}
		}
	}
	if conn != nil {
		return conn
	}
	w.f.voidcmd("TYPE A")
	listCmd := "LIST"
	if file != "" {
		pwd := w.f.pwd()
		err := ftpFailed(func() { w.f.cwd(file) })
		w.f.cwd(pwd)
		if err != nil {
			if err.Type == "error_perm" {
				raise("URLError", "<urlopen error ftp error: "+err.Msg+">")
			}
			panic(err)
		}
		listCmd = "LIST " + file
	}
	return w.f.transfer(listCmd)
}

// Runs fn, returning the ftplib error it raised and passing any other exception on
func ftpFailed(fn func()) (failed *Exception) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(*Exception); ok && strings.HasPrefix(e.Type, "error_") {
				failed = e
				return
			}
			panic(r)
		}
	}()
	fn()
	return nil
}

type ftpClient struct {
	conn net.Conn
	r    *bufio.Reader
}

func ftpConnect(host string, port int) *ftpClient {
	conn, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		raise("URLError", "<urlopen error ftp error: "+err.Error()+">")
	}
	f := &ftpClient{conn: conn, r: bufio.NewReader(conn)}
	f.getresp()
	return f
}

func (f *ftpClient) getline() string {
	line, err := f.r.ReadString('\n')
	if len(line) > 8193 {
		raise("error_proto", "got more than 8192 bytes")
	}
	if line == "" && err != nil {
		raise("EOFError", "")
	}
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
}

// getresp of ftplib, a multi line reply joined and its reply class raised as the ftplib error
func (f *ftpClient) getresp() string {
	line := f.getline()
	if len(line) >= 4 && line[3] == '-' {
		code := line[:3]
		for {
			next := f.getline()
			line += "\n" + next
			if strings.HasPrefix(next, code) && (len(next) < 4 || next[3] != '-') {
				break
			}
		}
	}
	switch {
	case strings.HasPrefix(line, "1"), strings.HasPrefix(line, "2"), strings.HasPrefix(line, "3"):
		return line
	case strings.HasPrefix(line, "4"):
		raise("error_temp", line)
	case strings.HasPrefix(line, "5"):
		raise("error_perm", line)
	}
	raise("error_proto", line)
	return ""
}

func (f *ftpClient) sendcmd(cmd string) string {
	if strings.ContainsAny(cmd, "\r\n") {
		raise("ValueError", "an illegal newline character should not be contained")
	}
	if r, bad := pystr.FirstUnencodableSurrogate(cmd); bad {
		raise("UnicodeEncodeError", fmt.Sprintf("'utf-8' codec can't encode character '\\u%04x'", r))
	}
	if _, err := io.WriteString(f.conn, cmd+"\r\n"); err != nil {
		raise("OSError", err.Error())
	}
	return f.getresp()
}

func (f *ftpClient) voidcmd(cmd string) string {
	resp := f.sendcmd(cmd)
	if !strings.HasPrefix(resp, "2") {
		raise("error_reply", resp)
	}
	return resp
}

// login of ftplib, anonymous with the anonymous@ password when no user is given
func (f *ftpClient) login(user, passwd string) {
	if user == "" {
		user = "anonymous"
	}
	if user == "anonymous" && (passwd == "" || passwd == "-") {
		passwd += "anonymous@"
	}
	resp := f.sendcmd("USER " + user)
	if strings.HasPrefix(resp, "3") {
		resp = f.sendcmd("PASS " + passwd)
	}
	if strings.HasPrefix(resp, "3") {
		resp = f.sendcmd("ACCT ")
	}
	if !strings.HasPrefix(resp, "2") {
		raise("error_reply", resp)
	}
}

func (f *ftpClient) cwd(dirname string) {
	if dirname == ".." {
		err := ftpFailed(func() { f.voidcmd("CDUP") })
		if err == nil {
			return
		}
		if err.Type != "error_perm" || !strings.HasPrefix(err.Msg, "500") {
			panic(err)
		}
	} else if dirname == "" {
		dirname = "."
	}
	f.voidcmd("CWD " + dirname)
}

// pwd of ftplib, the quoted directory of a 257 reply
func (f *ftpClient) pwd() string {
	resp := f.voidcmd("PWD")
	if !strings.HasPrefix(resp, "257") {
		return ""
	}
	if len(resp) < 5 || resp[3:5] != " \"" {
		return ""
	}
	var dir strings.Builder
	for i := 5; i < len(resp); {
		c := resp[i]
		i++
		if c == '"' {
			if i >= len(resp) || resp[i] != '"' {
				break
			}
			i++
		}
		dir.WriteByte(c)
	}
	return dir.String()
}

// ntransfercmd in passive mode, PASV for IPv4 control connections and EPSV otherwise
func (f *ftpClient) transfer(cmd string) net.Conn {
	peer := f.conn.RemoteAddr().(*net.TCPAddr)
	var port int
	if peer.IP.To4() != nil {
		resp := f.sendcmd("PASV")
		if !strings.HasPrefix(resp, "227") {
			raise("error_reply", resp)
		}
		m := pasvRe.FindStringSubmatch(resp)
		if m == nil {
			raise("error_proto", resp)
		}
		hi, _ := strconv.Atoi(m[5])
		lo, _ := strconv.Atoi(m[6])
		port = hi<<8 + lo
	} else {
		resp := f.sendcmd("EPSV")
		if !strings.HasPrefix(resp, "229") {
			raise("error_reply", resp)
		}
		left := strings.IndexByte(resp, '(')
		if left < 0 {
			raise("error_proto", resp)
		}
		right := strings.IndexByte(resp[left+1:], ')')
		if right < 0 {
			raise("error_proto", resp)
		}
		right += left + 1
		if right-1 < left+1 || resp[left+1] != resp[right-1] {
			raise("error_proto", resp)
		}
		parts := strings.Split(resp[left+1:right], string(resp[left+1]))
		if len(parts) != 5 {
			raise("error_proto", resp)
		}
		n, err := strconv.Atoi(parts[3])
		if err != nil {
			raise("ValueError", "invalid literal for int() with base 10: "+repr(parts[3]))
		}
		port = n
	}
	data, err := net.Dial("tcp", net.JoinHostPort(peer.IP.String(), strconv.Itoa(port)))
	if err != nil {
		raise("OSError", err.Error())
	}
	resp := ""
	if failed := ftpFailed(func() {
		resp = f.sendcmd(cmd)
		if strings.HasPrefix(resp, "2") {
			resp = f.getresp()
		}
		if !strings.HasPrefix(resp, "1") {
			raise("error_reply", resp)
		}
	}); failed != nil {
		data.Close()
		panic(failed)
	}
	return data
}
