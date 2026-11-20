package pyrequests

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/nickheyer/icebath/pkg/internal/netpref"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Error is a raised requests exception, Class its class name and Text its str
type Error struct {
	Class string
	Text  string
	// Reason is the urllib3 class under a MaxRetryError and DNS marks a name resolution cause
	Reason string
	DNS    bool
	Err    error
}

func (e *Error) Error() string { return e.Text }

func (e *Error) Unwrap() error { return e.Err }

// OSError is an OSError requests raises itself, outside the RequestException family its callers catch
type OSError struct{ Text string }

func (e *OSError) Error() string { return e.Text }

// Python exception class name the error stands for
func (e *OSError) PyTypeName() string { return "OSError" }

// Timeout is a requests timeout, each limit with the Python str of the value passed
type Timeout struct {
	Connect, Read         time.Duration
	ConnectText, ReadText string
}

// Seconds is the timeout requests builds from timeout=v for an int or float v
func Seconds(v any) Timeout {
	var d time.Duration
	switch n := v.(type) {
	case int:
		d = time.Duration(n) * time.Second
	case float64:
		d = time.Duration(n * float64(time.Second))
	}
	text := pyrepr.Str(v)
	return Timeout{Connect: d, Read: d, ConnectText: text, ReadText: text}
}

// Response is one answer read whole
type Response struct {
	Status int
	Header http.Header
	Body   []byte
	// Final URL after redirects, the url raise_for_status names
	URL string
}

// HTTPError requests raise_for_status raises for a client or server error status, nil for any other
func (r *Response) RaiseForStatus(rawURL string) error {
	kind := ""
	switch {
	case 400 <= r.Status && r.Status < 500:
		kind = "Client"
	case 500 <= r.Status && r.Status < 600:
		kind = "Server"
	default:
		return nil
	}
	return &Error{Class: "HTTPError", Text: fmt.Sprintf("%d %s Error: %s for url: %s", r.Status, kind, http.StatusText(r.Status), rawURL)}
}

type pool struct {
	https bool
	host  string
	port  int
}

func poolOf(u *url.URL) pool {
	p := pool{https: strings.EqualFold(u.Scheme, "https"), host: strings.ToLower(u.Hostname())}
	if _, err := fmt.Sscan(u.Port(), &p.port); err != nil || u.Port() == "" {
		p.port = 80
		if p.https {
			p.port = 443
		}
	}
	return p
}

func (p pool) String() string {
	return fmt.Sprintf("%sConnectionPool(host=%s, port=%d)", p.scheme(), pyrepr.Repr(p.host), p.port)
}

func (p pool) scheme() string {
	if p.https {
		return "HTTPS"
	}
	return "HTTP"
}

func (p pool) conn() string {
	return fmt.Sprintf("%sConnection(host=%s, port=%d)", p.scheme(), pyrepr.Repr(p.host), p.port)
}

// Connection repr with the id of a fresh object standing in for the CPython object address
func (p pool) connRepr() string {
	obj := new([16]byte)
	return fmt.Sprintf("<%s at %#x>", p.conn(), uintptr(unsafe.Pointer(obj)))
}

// PathURL is PreparedRequest.path_url, the path with its query and a slash for an empty path
func PathURL(u *url.URL) string {
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	return path
}

// Phase of the exchange a failure happened in, read from the client trace
type phase int32

const (
	phaseConnect phase = iota
	phaseTLS
	phaseExchange
)

var transports sync.Map

type transportKey struct {
	base          *http.Transport
	connect, read time.Duration
	verify        verifyPath
}

// Verify path a transport trusts with the stat identity that tells a replaced bundle apart, zero for none
type verifyPath struct {
	path          string
	dir           bool
	size, modTime int64
}

// Transport of one call, the base with the timeouts and a verify bundle that is on disk applied
func transportFor(base *http.Transport, t Timeout, bundle string) *http.Transport {
	key := transportKey{base: base, connect: t.Connect, read: t.Read}
	if bundle != "" {
		if info, err := os.Stat(bundle); err == nil {
			key.verify = verifyPath{bundle, info.IsDir(), info.Size(), info.ModTime().UnixNano()}
		}
	}
	if v, ok := transports.Load(key); ok {
		return v.(*http.Transport)
	}
	tr := base.Clone()
	tr.DialContext = netpref.DialContext((&net.Dialer{Timeout: t.Connect, KeepAlive: 30 * time.Second}).DialContext)
	tr.TLSHandshakeTimeout = t.Connect
	tr.ResponseHeaderTimeout = t.Read
	if key.verify.path != "" {
		tr.TLSClientConfig = VerifyTLSConfig(base.TLSClientConfig, key.verify.path, key.verify.dir)
	}
	v, _ := transports.LoadOrStore(key, tr)
	return v.(*http.Transport)
}

// Send performs one requests call, every transport failure raised as the requests exception it maps to
func Send(ctx context.Context, base *http.Transport, t Timeout, req *http.Request) (*Response, error) {
	return sendWith(ctx, base, t, req, true, checkRedirect)
}

// SendNoRedirects is Send with allow_redirects False, a redirect answer returned as the response
func SendNoRedirects(ctx context.Context, base *http.Transport, t Timeout, req *http.Request) (*Response, error) {
	return sendWith(ctx, base, t, req, true, func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse })
}

// SendNoEnv is Send for a client that is not a requests session trusting its environment, the verify bundle variables left unread
func SendNoEnv(ctx context.Context, base *http.Transport, t Timeout, req *http.Request) (*Response, error) {
	return sendWith(ctx, base, t, req, false, checkRedirect)
}

// One requests call following redirects as redirect decides, trustEnv merging the environment verify bundle into default verification
func sendWith(ctx context.Context, base *http.Transport, t Timeout, req *http.Request, trustEnv bool, redirect func(*http.Request, []*http.Request) error) (*Response, error) {
	bundle := ""
	if trustEnv && (base.TLSClientConfig == nil || !base.TLSClientConfig.InsecureSkipVerify) {
		bundle = EnvCABundle()
	}
	if err := certVerify(req.URL, bundle); err != nil {
		return nil, err
	}
	follow := redirect
	if bundle != "" {
		follow = func(next *http.Request, via []*http.Request) error {
			if err := redirect(next, via); err != nil {
				return err
			}
			return certVerify(next.URL, bundle)
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var at atomic.Int32
	trace := &httptrace.ClientTrace{
		GetConn:           func(string) { at.Store(int32(phaseConnect)) },
		TLSHandshakeStart: func() { at.Store(int32(phaseTLS)) },
		GotConn:           func(httptrace.GotConnInfo) { at.Store(int32(phaseExchange)) },
	}
	req = req.WithContext(httptrace.WithClientTrace(ctx, trace))
	tr := transportFor(base, t, bundle)
	var proxy *url.URL
	if tr.Proxy != nil {
		proxy, _ = tr.Proxy(req)
	}
	resp, err := (&http.Client{Transport: tr, CheckRedirect: follow}).Do(req)
	if err != nil {
		if ctx.Err() != nil && errors.Is(err, context.Canceled) {
			return nil, err
		}
		if errors.Is(err, errTooManyRedirects) {
			return nil, &Error{Class: "TooManyRedirects", Text: "Exceeded 30 redirects.", Err: err}
		}
		var missing *OSError
		if errors.As(err, &missing) {
			return nil, missing
		}
		target := req.URL
		var ue *url.Error
		if errors.As(err, &ue) {
			if hop, perr := url.Parse(ue.URL); perr == nil {
				target = hop
				if tr.Proxy != nil {
					proxy, _ = tr.Proxy(&http.Request{URL: hop})
				}
			}
		}
		return nil, transportError(target, proxy, phase(at.Load()), t, err)
	}
	defer resp.Body.Close()
	var idleFired atomic.Bool
	idle := t.Read
	if idle <= 0 {
		idle = time.Duration(math.MaxInt64)
	}
	timer := time.AfterFunc(idle, func() {
		idleFired.Store(true)
		cancel()
	})
	defer timer.Stop()
	var buf bytes.Buffer
	chunk := make([]byte, 32<<10)
	for {
		n, rerr := resp.Body.Read(chunk)
		if n > 0 {
			buf.Write(chunk[:n])
			timer.Reset(idle)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if idleFired.Load() {
				return nil, &Error{Class: "ConnectionError", Text: poolFor(req.URL, proxy).String() + ": Read timed out.", Reason: "ReadTimeoutError", Err: rerr}
			}
			if ctx.Err() != nil && errors.Is(rerr, context.Canceled) {
				return nil, rerr
			}
			return nil, bodyError(resp, buf.Len(), rerr)
		}
	}
	final := req.URL
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL
	}
	return &Response{Status: resp.StatusCode, Header: resp.Header, Body: buf.Bytes(), URL: final.String()}, nil
}

var errTooManyRedirects = errors.New("Exceeded 30 redirects.")

// Redirects followed like a requests Session, at most thirty hops
func checkRedirect(_ *http.Request, via []*http.Request) error {
	if len(via) > 30 {
		return errTooManyRedirects
	}
	return nil
}

// Pool a request ran on, the proxy pool for a plain http target sent through a proxy
func poolFor(target, proxy *url.URL) pool {
	if proxy != nil && !strings.EqualFold(target.Scheme, "https") {
		return poolOf(proxy)
	}
	return poolOf(target)
}

// Request url urllib3 reports, the full url for a plain http target sent through a proxy
func requestURL(target, proxy *url.URL) string {
	if proxy != nil && !strings.EqualFold(target.Scheme, "https") {
		return target.String()
	}
	return PathURL(target)
}

// Maps a failed round trip to the requests exception of the same failure
func transportError(target, proxy *url.URL, at phase, t Timeout, err error) *Error {
	p := poolFor(target, proxy)
	retry := func(class, reasonClass, reasonRepr string, dns bool) *Error {
		text := fmt.Sprintf("%s: Max retries exceeded with url: %s (Caused by %s)", p, requestURL(target, proxy), reasonRepr)
		return &Error{Class: class, Text: text, Reason: reasonClass, DNS: dns, Err: err}
	}
	connPool := poolOf(target)
	if proxy != nil {
		connPool = poolOf(proxy)
		connPool.https = strings.EqualFold(proxy.Scheme, "https")
	}
	var ne net.Error
	timedOut := errors.As(err, &ne) && ne.Timeout()
	switch {
	case at == phaseConnect:
		var reasonClass, reasonRepr string
		var dns bool
		var dnsErr *net.DNSError
		switch {
		case errors.As(err, &dnsErr):
			reasonClass, dns = "NameResolutionError", true
			reasonRepr = "NameResolutionError(" + pyrepr.Repr(fmt.Sprintf("%s: Failed to resolve '%s' (%s)", connPool.conn(), connPool.host, pyhttpx.OSErrorText(err))) + ")"
		case timedOut:
			reasonClass = "ConnectTimeoutError"
			reasonRepr = fmt.Sprintf("ConnectTimeoutError(%s, %s)", connPool.connRepr(), pyrepr.Repr(fmt.Sprintf("Connection to %s timed out. (connect timeout=%s)", connPool.host, t.ConnectText)))
		default:
			reasonClass = "NewConnectionError"
			reasonRepr = "NewConnectionError(" + pyrepr.Repr(connPool.conn()+": Failed to establish a new connection: "+pyhttpx.OSErrorText(err)) + ")"
		}
		if proxy != nil {
			return retry("ProxyError", "ProxyError", "ProxyError('Unable to connect to proxy', "+reasonRepr+")", dns)
		}
		if reasonClass == "ConnectTimeoutError" {
			return retry("ConnectTimeout", reasonClass, reasonRepr, false)
		}
		return retry("ConnectionError", reasonClass, reasonRepr, dns)
	case at == phaseTLS:
		if timedOut || strings.Contains(err.Error(), "TLS handshake timeout") {
			return &Error{Class: "ReadTimeout", Text: fmt.Sprintf("%s: Read timed out. (read timeout=%s)", p, t.ConnectText), Reason: "ReadTimeoutError", Err: err}
		}
		if reset, ok := osErrorRepr(err); ok {
			return &Error{Class: "ConnectionError", Text: "('Connection aborted.', " + reset + ")", Reason: "ProtocolError", Err: err}
		}
		return retry("SSLError", "SSLError", "SSLError("+sslErrorRepr(target, err)+")", false)
	}
	if timedOut || strings.Contains(err.Error(), "timeout awaiting response headers") {
		return &Error{Class: "ReadTimeout", Text: fmt.Sprintf("%s: Read timed out. (read timeout=%s)", p, t.ReadText), Reason: "ReadTimeoutError", Err: err}
	}
	cause := "RemoteDisconnected('Remote end closed connection without response')"
	if r, ok := osErrorRepr(err); ok {
		cause = r
	}
	return &Error{Class: "ConnectionError", Text: "('Connection aborted.', " + cause + ")", Reason: "ProtocolError", Err: err}
}

// Maps a failed body read to the ChunkedEncodingError iter_content raises
func bodyError(resp *http.Response, read int, err error) *Error {
	if errors.Is(err, io.ErrUnexpectedEOF) {
		if resp.ContentLength >= 0 && len(resp.TransferEncoding) == 0 {
			inc := fmt.Sprintf("IncompleteRead(%d bytes read, %d more expected)", read, resp.ContentLength-int64(read))
			return &Error{Class: "ChunkedEncodingError", Text: "(" + pyrepr.Repr("Connection broken: "+inc) + ", " + inc + ")", Reason: "ProtocolError", Err: err}
		}
		return &Error{Class: "ChunkedEncodingError", Text: "Response ended prematurely", Reason: "ProtocolError", Err: err}
	}
	cause, ok := osErrorRepr(err)
	if !ok {
		cause = "OSError(" + pyrepr.Repr(pyhttpx.OSErrorText(err)) + ")"
	}
	return &Error{Class: "ChunkedEncodingError", Text: "(" + pyrepr.Repr("Connection broken: "+cause) + ", " + cause + ")", Reason: "ProtocolError", Err: err}
}

// repr of the OSError subclass CPython raises for a socket errno, false when the cause has no errno
func osErrorRepr(err error) (string, bool) {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return "", false
	}
	class := "OSError"
	switch errno {
	case syscall.ECONNRESET:
		class = "ConnectionResetError"
	case syscall.ECONNABORTED:
		class = "ConnectionAbortedError"
	case syscall.ECONNREFUSED:
		class = "ConnectionRefusedError"
	case syscall.EPIPE:
		class = "BrokenPipeError"
	case syscall.ETIMEDOUT:
		class = "TimeoutError"
	}
	text := pyhttpx.OSErrorText(errno)
	msg := text[strings.Index(text, "] ")+2:]
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("%s(%d, %s, None, %d, None)", class, int(errno), pyrepr.Repr(msg), int(errno)), true
	}
	return fmt.Sprintf("%s(%d, %s)", class, int(errno), pyrepr.Repr(msg)), true
}

// Line of the CPython 3.14 _ssl.c handshake call every OpenSSL error names
const sslLine = "(_ssl.c:1082)"

var alertReasons = map[tls.AlertError][2]string{
	10: {"SSLV3_ALERT_UNEXPECTED_MESSAGE", "ssl/tls alert unexpected message"}, 20: {"SSLV3_ALERT_BAD_RECORD_MAC", "ssl/tls alert bad record mac"},
	21: {"TLSV1_ALERT_DECRYPTION_FAILED", "tlsv1 alert decryption failed"}, 22: {"TLSV1_ALERT_RECORD_OVERFLOW", "tlsv1 alert record overflow"},
	30: {"SSLV3_ALERT_DECOMPRESSION_FAILURE", "ssl/tls alert decompression failure"}, 40: {"SSLV3_ALERT_HANDSHAKE_FAILURE", "ssl/tls alert handshake failure"},
	41: {"SSLV3_ALERT_NO_CERTIFICATE", "ssl/tls alert no certificate"}, 42: {"SSLV3_ALERT_BAD_CERTIFICATE", "ssl/tls alert bad certificate"},
	43: {"SSLV3_ALERT_UNSUPPORTED_CERTIFICATE", "ssl/tls alert unsupported certificate"}, 44: {"SSLV3_ALERT_CERTIFICATE_REVOKED", "ssl/tls alert certificate revoked"},
	45: {"SSLV3_ALERT_CERTIFICATE_EXPIRED", "ssl/tls alert certificate expired"}, 46: {"SSLV3_ALERT_CERTIFICATE_UNKNOWN", "ssl/tls alert certificate unknown"},
	47: {"SSLV3_ALERT_ILLEGAL_PARAMETER", "ssl/tls alert illegal parameter"}, 48: {"TLSV1_ALERT_UNKNOWN_CA", "tlsv1 alert unknown ca"},
	49: {"TLSV1_ALERT_ACCESS_DENIED", "tlsv1 alert access denied"}, 50: {"TLSV1_ALERT_DECODE_ERROR", "tlsv1 alert decode error"},
	51: {"TLSV1_ALERT_DECRYPT_ERROR", "tlsv1 alert decrypt error"}, 60: {"TLSV1_ALERT_EXPORT_RESTRICTION", "tlsv1 alert export restriction"},
	70: {"TLSV1_ALERT_PROTOCOL_VERSION", "tlsv1 alert protocol version"}, 71: {"TLSV1_ALERT_INSUFFICIENT_SECURITY", "tlsv1 alert insufficient security"},
	80: {"TLSV1_ALERT_INTERNAL_ERROR", "tlsv1 alert internal error"}, 86: {"TLSV1_ALERT_INAPPROPRIATE_FALLBACK", "tlsv1 alert inappropriate fallback"},
	90: {"TLSV1_ALERT_USER_CANCELLED", "tlsv1 alert user cancelled"}, 100: {"TLSV1_ALERT_NO_RENEGOTIATION", "tlsv1 alert no renegotiation"},
	109: {"TLSV13_ALERT_MISSING_EXTENSION", "tlsv13 alert missing extension"}, 110: {"TLSV1_UNSUPPORTED_EXTENSION", "tlsv1 unsupported extension"},
	111: {"TLSV1_CERTIFICATE_UNOBTAINABLE", "tlsv1 certificate unobtainable"}, 112: {"TLSV1_UNRECOGNIZED_NAME", "tlsv1 unrecognized name"},
	113: {"TLSV1_BAD_CERTIFICATE_STATUS_RESPONSE", "tlsv1 bad certificate status response"}, 114: {"TLSV1_BAD_CERTIFICATE_HASH_VALUE", "tlsv1 bad certificate hash value"},
	115: {"TLSV1_ALERT_UNKNOWN_PSK_IDENTITY", "tlsv1 alert unknown psk identity"}, 116: {"TLSV13_ALERT_CERTIFICATE_REQUIRED", "tlsv13 alert certificate required"},
	120: {"TLSV1_ALERT_NO_APPLICATION_PROTOCOL", "tlsv1 alert no application protocol"},
}

// Alert codes by the text crypto/tls prints for a received alert
var goAlertCodes = map[string]int{
	"unexpected message": 10, "bad record MAC": 20, "decryption failed": 21, "record overflow": 22, "decompression failure": 30,
	"handshake failure": 40, "bad certificate": 42, "unsupported certificate": 43, "revoked certificate": 44, "expired certificate": 45,
	"unknown certificate": 46, "illegal parameter": 47, "unknown certificate authority": 48, "access denied": 49,
	"error decoding message": 50, "error decrypting message": 51, "export restriction": 60, "protocol version not supported": 70,
	"insufficient security level": 71, "internal error": 80, "inappropriate fallback": 86, "user canceled": 90, "no renegotiation": 100,
	"missing extension": 109, "unsupported extension": 110, "certificate unobtainable": 111, "unrecognized name": 112,
	"bad certificate status response": 113, "bad certificate hash value": 114, "unknown PSK identity": 115, "certificate required": 116,
	"no application protocol": 120,
}

func selfSigned(c *x509.Certificate) bool {
	return bytes.Equal(c.RawIssuer, c.RawSubject) && c.CheckSignatureFrom(c) == nil
}

// repr of the ssl module exception a failed handshake raises
func sslErrorRepr(target *url.URL, err error) string {
	var load *bundleLoadError
	if errors.As(err, &load) {
		return load.repr
	}
	verify := func(reason string) string {
		return "SSLCertVerificationError(1, " + pyrepr.Repr("[SSL: CERTIFICATE_VERIFY_FAILED] certificate verify failed: "+reason+" "+sslLine) + ")"
	}
	var host x509.HostnameError
	var hostPtr *x509.HostnameError
	if errors.As(err, &hostPtr) {
		host = *hostPtr
	} else if !errors.As(err, &host) {
		host.Host = ""
	}
	if host.Host != "" {
		kind := "Hostname mismatch"
		if net.ParseIP(strings.Trim(host.Host, "[]")) != nil {
			kind = "IP address mismatch"
		}
		return verify(fmt.Sprintf("%s, certificate is not valid for '%s'.", kind, host.Host))
	}
	var unknown x509.UnknownAuthorityError
	var cve *tls.CertificateVerificationError
	if errors.As(err, &unknown) || errors.As(err, &cve) && errors.As(cve.Err, &unknown) {
		chain := []*x509.Certificate{}
		if cve != nil {
			chain = cve.UnverifiedCertificates
		}
		switch {
		case len(chain) > 0 && selfSigned(chain[0]):
			return verify("self-signed certificate")
		case len(chain) > 1 && selfSigned(chain[len(chain)-1]):
			return verify("self-signed certificate in certificate chain")
		case len(chain) == 0 && unknown.Cert != nil && selfSigned(unknown.Cert):
			return verify("self-signed certificate")
		}
		return verify("unable to get local issuer certificate")
	}
	var invalid x509.CertificateInvalidError
	if errors.As(err, &invalid) && invalid.Reason == x509.Expired && invalid.Cert != nil {
		if time.Now().Before(invalid.Cert.NotBefore) {
			return verify("certificate is not yet valid")
		}
		return verify("certificate has expired")
	}
	var record tls.RecordHeaderError
	if errors.As(err, &record) || strings.Contains(err.Error(), "server gave HTTP response to HTTPS client") {
		return "SSLError(1, " + pyrepr.Repr("[SSL: WRONG_VERSION_NUMBER] wrong version number "+sslLine) + ")"
	}
	if desc, ok := strings.CutPrefix(err.Error()[max(0, strings.Index(err.Error(), "remote error: tls: ")):], "remote error: tls: "); ok {
		code, known := goAlertCodes[desc]
		if !known {
			_, scanErr := fmt.Sscanf(desc, "alert(%d)", &code)
			known = scanErr == nil
		}
		if r, ok := alertReasons[tls.AlertError(code)]; ok && known {
			return "SSLError(1, " + pyrepr.Repr("[SSL: "+r[0]+"] "+r[1]+" "+sslLine) + ")"
		}
		return "SSLError(1, " + pyrepr.Repr("[SSL] unknown error "+sslLine) + ")"
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "SSLEOFError(8, " + pyrepr.Repr("[SSL: UNEXPECTED_EOF_WHILE_READING] EOF occurred in violation of protocol "+sslLine) + ")"
	}
	return sslLocalErrorRepr(err)
}

// Handshake failure the Go stack detected itself, its description in the CPython lib and errstr layout
func sslLocalErrorRepr(err error) string {
	text := err.Error()
	if i := strings.LastIndex(text, "tls: "); i >= 0 {
		text = text[i+len("tls: "):]
	}
	return "SSLError(1, " + pyrepr.Repr("[SSL] "+text+" "+sslLine) + ")"
}

// Verify bundle requests merges from the environment, REQUESTS_CA_BUNDLE then CURL_CA_BUNDLE, empty keeping the default
func EnvCABundle() string {
	if bundle := os.Getenv("REQUESTS_CA_BUNDLE"); bundle != "" {
		return bundle
	}
	return os.Getenv("CURL_CA_BUNDLE")
}

// cert_verify of one hop, the OSError requests raises before an https request whose verify path is not on disk
func certVerify(u *url.URL, bundle string) error {
	if bundle == "" || !strings.EqualFold(u.Scheme, "https") {
		return nil
	}
	if _, err := os.Stat(bundle); err != nil {
		return &OSError{"Could not find a suitable TLS CA certificate bundle, invalid path: " + bundle}
	}
	return nil
}

// Line of the CPython 3.14 _ssl.c load_verify_locations call a bundle OpenSSL cannot use names
const sslLoadLine = "(_ssl.c:4419)"

// Failure to load a verify bundle, repr the exception load_verify_locations raises for it
type bundleLoadError struct{ repr string }

func (e *bundleLoadError) Error() string { return e.repr }

// OpenSSL capath entry names, a subject hash and a sequence number
var hashedCertName = regexp.MustCompile(`^[0-9a-f]{8}\.[0-9]+$`)

// TLS config verifying with a bundle file or hashed capath directory on top of the roots base trusts, an unusable file failing every handshake with its load error
func VerifyTLSConfig(base *tls.Config, bundle string, dir bool) *tls.Config {
	cfg := &tls.Config{}
	if base != nil {
		cfg = base.Clone()
	}
	var pool *x509.CertPool
	if cfg.RootCAs != nil {
		pool = cfg.RootCAs.Clone()
	} else if pool, _ = x509.SystemCertPool(); pool == nil {
		pool = x509.NewCertPool()
	}
	if dir {
		entries, _ := os.ReadDir(bundle)
		for _, entry := range entries {
			if hashedCertName.MatchString(entry.Name()) {
				if raw, err := os.ReadFile(filepath.Join(bundle, entry.Name())); err == nil {
					pool.AppendCertsFromPEM(raw)
				}
			}
		}
		cfg.RootCAs = pool
		return cfg
	}
	failed := ""
	raw, err := os.ReadFile(bundle)
	switch {
	case errors.Is(err, fs.ErrPermission):
		failed = "PermissionError(13, 'Permission denied')"
	case errors.Is(err, fs.ErrNotExist):
		failed = "FileNotFoundError(2, 'No such file or directory')"
	case err != nil:
		if repr, ok := osErrorRepr(err); ok {
			failed = repr
		} else {
			failed = "OSError(" + pyrepr.Repr(err.Error()) + ")"
		}
	case !pool.AppendCertsFromPEM(raw):
		failed = "SSLError(136, " + pyrepr.Repr("[X509: NO_CERTIFICATE_OR_CRL_FOUND] no certificate or crl found "+sslLoadLine) + ")"
	}
	if failed != "" {
		cfg.InsecureSkipVerify = true
		cfg.VerifyConnection = func(tls.ConnectionState) error { return &bundleLoadError{failed} }
		return cfg
	}
	cfg.RootCAs = pool
	return cfg
}
