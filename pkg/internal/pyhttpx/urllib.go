package pyhttpx

import (
	"encoding/base64"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// Header dicts of a urllib.request.Request, names capitalized as add_header stores them, the unredirected ones apart
type URLLibRequest struct {
	headers      []HeaderLine
	unredirected []HeaderLine
}

// Request built with the headers argument in order, each stored under its capitalized name
func NewURLLibRequest(headers ...HeaderLine) *URLLibRequest {
	r := &URLLibRequest{}
	for _, l := range headers {
		r.AddHeader(l.Name, l.Value)
	}
	return r
}

// Python dict assignment, an existing key keeping its slot
func dictSet(lines []HeaderLine, name, value string) []HeaderLine {
	for i, l := range lines {
		if l.Name == name {
			lines[i].Value = value
			return lines
		}
	}
	return append(lines, HeaderLine{name, value})
}

// Python dict pop of one exact key
func dictDel(lines []HeaderLine, name string) []HeaderLine {
	kept := lines[:0]
	for _, l := range lines {
		if l.Name != name {
			kept = append(kept, l)
		}
	}
	return kept
}

// Stores the value under the capitalized name in the redirected headers
func (r *URLLibRequest) AddHeader(name, value string) {
	r.headers = dictSet(r.headers, pystr.Capitalize(name), value)
}

// Stores the value under the capitalized name in the headers a redirect does not carry
func (r *URLLibRequest) AddUnredirectedHeader(name, value string) {
	r.unredirected = dictSet(r.unredirected, pystr.Capitalize(name), value)
}

// Whether either dict holds the exact name
func (r *URLLibRequest) HasHeader(name string) bool {
	_, ok := r.Header(name)
	return ok
}

// Value under the exact name, the redirected headers before the unredirected ones as get_header reads them
func (r *URLLibRequest) Header(name string) (string, bool) {
	for _, set := range [][]HeaderLine{r.headers, r.unredirected} {
		for _, l := range set {
			if l.Name == name {
				return l.Value, true
			}
		}
	}
	return "", false
}

// Pops the exact name from both dicts
func (r *URLLibRequest) RemoveHeader(name string) {
	r.headers = dictDel(r.headers, name)
	r.unredirected = dictDel(r.unredirected, name)
}

// Keeps only the names whose lowercase form is in keep, both dicts, as _strip_headers drops the rest
func (r *URLLibRequest) StripHeaders(keep map[string]bool) {
	for _, l := range r.HeaderItems() {
		if !keep[strings.ToLower(l.Name)] {
			r.RemoveHeader(l.Name)
		}
	}
}

// Unredirected headers then the redirected ones merged as one dict, header_items order
func (r *URLLibRequest) HeaderItems() []HeaderLine {
	out := append([]HeaderLine(nil), r.unredirected...)
	for _, l := range r.headers {
		out = dictSet(out, l.Name, l.Value)
	}
	return out
}

// Request of a redirect hop, the redirected headers without Content-Length and Content-Type as redirect_request copies them
func (r *URLLibRequest) Redirected() *URLLibRequest {
	out := &URLLibRequest{}
	for _, l := range r.headers {
		if lower := strings.ToLower(l.Name); lower != "content-length" && lower != "content-type" {
			out.headers = append(out.headers, l)
		}
	}
	return out
}

// Copy whose dicts edit apart from these
func (r *URLLibRequest) Clone() *URLLibRequest {
	return &URLLibRequest{headers: append([]HeaderLine(nil), r.headers...), unredirected: append([]HeaderLine(nil), r.unredirected...)}
}

// Fills the unredirected body headers, Host and opener addheaders the way AbstractHTTPHandler.do_request_ does
func (r *URLLibRequest) DoRequest(host string, data []byte, hasData bool, addheaders []HeaderLine) {
	if hasData {
		if !r.HasHeader("Content-type") {
			r.AddUnredirectedHeader("Content-type", "application/x-www-form-urlencoded")
		}
		if !r.HasHeader("Content-length") && !r.HasHeader("Transfer-encoding") {
			r.AddUnredirectedHeader("Content-length", strconv.Itoa(len(data)))
		}
	}
	if !r.HasHeader("Host") {
		r.AddUnredirectedHeader("Host", host)
	}
	for _, l := range addheaders {
		name := pystr.Capitalize(l.Name)
		if !r.HasHeader(name) {
			r.AddUnredirectedHeader(name, l.Value)
		}
	}
}

// Adds the Basic credentials ProxyHandler sends when the proxy URL holds a user and a password
func (r *URLLibRequest) AddProxyAuthorization(proxyURL *url.URL) {
	if proxyURL == nil || proxyURL.User == nil {
		return
	}
	pw, ok := proxyURL.User.Password()
	if user := proxyURL.User.Username(); user != "" && ok && pw != "" {
		r.AddHeader("Proxy-authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+pw)))
	}
}

// Host line http.client putrequest writes itself, the port left off when it is the default of the scheme
func URLLibPutHost(u *url.URL) string {
	host := u.Hostname()
	port := u.Port()
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port == "" || (port == "80" && u.Scheme == "http") || (port == "443" && u.Scheme == "https") {
		return host
	}
	return host + ":" + port
}

// Head lines http.client writes for the request, Accept-Encoding identity unless set, the do_open dict title cased with Connection close, a body length last
func (r *URLLibRequest) WireLines(method string, putHost string, tunnel bool, body []byte, hasBody bool) *Headers {
	merged := append([]HeaderLine(nil), r.unredirected...)
	for _, l := range r.headers {
		merged = dictSet(merged, l.Name, l.Value)
	}
	merged = dictSet(merged, "Connection", "close")
	var titled []HeaderLine
	for _, l := range merged {
		titled = dictSet(titled, pystr.Title(l.Name), l.Value)
	}
	if tunnel {
		titled = dictDel(titled, "Proxy-Authorization")
	}
	names := map[string]bool{}
	for _, l := range titled {
		names[strings.ToLower(l.Name)] = true
	}
	var out []HeaderLine
	if !names["host"] {
		out = append(out, HeaderLine{"Host", putHost})
	}
	if !names["accept-encoding"] {
		out = append(out, HeaderLine{"Accept-Encoding", "identity"})
	}
	out = append(out, titled...)
	if !names["content-length"] && !names["transfer-encoding"] {
		switch {
		case hasBody:
			out = append(out, HeaderLine{"Content-Length", strconv.Itoa(len(body))})
		case method == "POST" || method == "PUT" || method == "PATCH":
			out = append(out, HeaderLine{"Content-Length", "0"})
		}
	}
	return NewHeaders(out...).HostInPlace()
}

// Proxy urllib ProxyHandler sends a request for u through, nil when none is set or proxy_bypass exempts the host
func URLLibProxy(u *url.URL) *url.URL {
	proxies := pyurl.Getproxies()
	raw, ok := proxies[strings.ToLower(u.Scheme)]
	if !ok || raw == "" || u.Host == "" || urllibProxyBypass(u.Host) {
		return nil
	}
	if !strings.Contains(raw, "://") {
		raw = u.Scheme + "://" + raw
	}
	proxyURL, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	return proxyURL
}

// urllib proxy_bypass, the no_proxy list when any proxy variable is set and the operating system list otherwise
func urllibProxyBypass(host string) bool {
	if env := pyurl.EnvironProxies(os.Environ()); len(env) > 0 {
		return pyurl.ProxyBypassEnvironment(host, env)
	}
	return systemProxyBypass(host)
}

// Host and numeric port the way urllib _splitport splits them, an empty port when none follows
func splitPort(host string) (string, string) {
	if i := strings.LastIndex(host, ":"); i >= 0 && strings.Trim(host[i+1:], "0123456789") == "" {
		return host[:i], host[i+1:]
	}
	return host, ""
}
