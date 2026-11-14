package gcp

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/golang-jwt/jwt/v5"

	"github.com/nickheyer/icebath/internal/httpclient"
)

const (
	formType = "application/x-www-form-urlencoded"
	jsonType = "application/json"
)

type client struct {
	api    *http.Client
	direct *http.Client
	http   httpclient.Options
}

func newClient(o httpclient.Options) (*client, error) {
	apiOpts := o
	apiOpts.Timeouts = httpclient.EachPhase(requestTimeout)
	api, err := httpclient.New(apiOpts)
	if err != nil {
		return nil, err
	}
	directOpts := o
	directOpts.Proxy = httpclient.ProxyOptions{IgnoreEnv: true}
	directOpts.Timeouts = httpclient.EachPhase(requestTimeout)
	direct, err := httpclient.New(directOpts)
	if err != nil {
		return nil, err
	}
	return &client{api: api.Client(), direct: direct.Client(), http: o}, nil
}

func (c *client) send(ctx context.Context, hc *http.Client, timeout time.Duration, method, target string, header http.Header, body []byte) (int, http.Header, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return 0, nil, nil, err
	}
	for name, values := range header {
		req.Header[name] = values
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, replyLimit+1))
	if err != nil {
		return 0, nil, nil, err
	}
	if len(data) > replyLimit {
		return 0, nil, nil, errors.New("reply is larger than 1 MiB")
	}
	return resp.StatusCode, resp.Header, data, nil
}

type document map[string]json.RawMessage

func parseDocument(data []byte) (document, bool) {
	var doc document
	if err := json.Unmarshal(data, &doc); err != nil || doc == nil {
		return nil, false
	}
	return doc, true
}

func (d document) has(key string) bool {
	_, ok := d[key]
	return ok
}

func (d document) text(key string) string {
	var s string
	if raw, ok := d[key]; ok && json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

func (d document) object(key string) (document, bool) {
	raw, ok := d[key]
	if !ok {
		return nil, false
	}
	return parseDocument(raw)
}

func (d document) number(key string) (float64, bool) {
	raw, ok := d[key]
	if !ok {
		return 0, false
	}
	var n float64
	if json.Unmarshal(raw, &n) == nil {
		return n, true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
			return float64(v), true
		}
	}
	return 0, false
}

func (d document) strings(key string) []string {
	raw, ok := d[key]
	if !ok {
		return nil
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return list
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.Split(s, " ")
	}
	return nil
}

func (d document) missing(keys ...string) []string {
	var out []string
	for _, k := range keys {
		if !d.has(k) {
			out = append(out, k)
		}
	}
	return out
}

func oauthFailure(kind string, status int, reply document) *Error {
	e := &Error{Kind: kind, Status: status}
	code, desc := reply.text("error"), reply.text("error_description")
	e.Code = code
	switch status {
	case http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusRequestTimeout, http.StatusTooManyRequests:
		e.Retryable = true
	}
	for _, v := range []string{code, desc} {
		if v == "internal_failure" || v == "server_error" || v == "temporarily_unavailable" {
			e.Retryable = true
		}
	}
	switch {
	case code != "" && desc != "":
		e.Message = code + ": " + desc
	case code != "":
		e.Message = code
	default:
		e.Message = "token endpoint refused the request"
	}
	return e
}

func (c *client) tokenRequest(ctx context.Context, hc *http.Client, kind, endpoint, contentType string, body []byte) (document, error) {
	wait := time.Second
	var last *Error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			if err := sleep(ctx, jittered(wait)); err != nil {
				return nil, err
			}
			wait *= 2
		}
		status, _, data, err := c.send(ctx, hc, requestTimeout, http.MethodPost, endpoint, http.Header{"Content-Type": {contentType}}, body)
		if err != nil {
			return nil, &Error{Kind: kind, Message: "token endpoint could not be reached: " + httpclient.Message(err), Retryable: true, Err: err}
		}
		reply, ok := parseDocument(data)
		if status == http.StatusOK {
			if !ok {
				return nil, &Error{Kind: kind, Status: status, Message: "token endpoint reply is not a JSON object"}
			}
			return reply, nil
		}
		last = oauthFailure(kind, status, reply)
		if reply.text("error") == "invalid_grant" {
			if sub := reply.text("error_subtype"); sub == "invalid_rapt" || sub == "rapt_required" {
				last.Err = ErrReauthRequired
			}
		}
		if !last.Retryable {
			break
		}
	}
	return nil, last
}

func (c *client) allowedFor(kind string, target func(ctx context.Context) (string, error)) func(ctx context.Context, token string) (string, error) {
	return func(ctx context.Context, token string) (string, error) {
		endpoint, err := target(ctx)
		if err != nil {
			return "", err
		}
		return c.lookupBoundary(ctx, kind, endpoint, token)
	}
}

func (c *client) lookupBoundary(ctx context.Context, kind, endpoint, token string) (string, error) {
	wait := time.Second
	var last *Error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			if err := sleep(ctx, jittered(wait)); err != nil {
				return "", err
			}
			wait *= 2
		}
		status, _, data, err := c.send(ctx, c.api, requestTimeout, http.MethodGet, endpoint, http.Header{"Authorization": {"Bearer " + token}}, nil)
		if err != nil {
			return "", &Error{Kind: kind, Message: "trust boundary lookup could not be reached: " + httpclient.Message(err), Retryable: true, Err: err}
		}
		reply, ok := parseDocument(data)
		if status == http.StatusOK {
			if !ok || !reply.has("encodedLocations") {
				return "", &Error{Kind: kind, Status: status, Message: "Invalid trust boundary info: the lookup reply carries no encodedLocations"}
			}
			return reply.text("encodedLocations"), nil
		}
		last = oauthFailure(kind, status, reply)
		if reply.text("error") == "" {
			last.Message = "trust boundary lookup refused the request" + googleErrorText(data)
		}
		last.Err = errBoundaryRefresh
		if !last.Retryable {
			break
		}
	}
	return "", last
}

func accessToken(kind string, reply document, now time.Time) (string, time.Time, error) {
	token := reply.text("access_token")
	if token == "" {
		return "", time.Time{}, &Error{Kind: kind, Message: "token endpoint reply carried no access token"}
	}
	if !reply.has("expires_in") || string(reply["expires_in"]) == "null" {
		return token, time.Time{}, nil
	}
	seconds, ok := reply.number("expires_in")
	if !ok {
		return "", time.Time{}, &Error{Kind: kind, Message: "token endpoint reply carried an unreadable expires_in"}
	}
	return token, now.Add(time.Duration(seconds * float64(time.Second))), nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func jittered(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.9 + rand.Float64()*0.2))
}

type pair struct{ key, value string }

func encodeForm(pairs ...pair) []byte {
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(p.key))
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(p.value))
	}
	return []byte(b.String())
}

func quotePath(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9', ch == '_', ch == '.', ch == '-', ch == '~', ch == '/':
			b.WriteByte(ch)
		default:
			b.WriteByte('%')
			b.WriteByte(hex[ch>>4])
			b.WriteByte(hex[ch&15])
		}
	}
	return b.String()
}

type member struct {
	key   string
	value any
}

type object []member

func writeJSON(b *strings.Builder, v any, itemSep, keySep string) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case string:
		writeJSONString(b, x)
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case int:
		b.WriteString(strconv.Itoa(x))
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case []string:
		if x == nil {
			b.WriteString("null")
			return
		}
		b.WriteByte('[')
		for i, s := range x {
			if i > 0 {
				b.WriteString(itemSep)
			}
			writeJSONString(b, s)
		}
		b.WriteByte(']')
	case []object:
		b.WriteByte('[')
		for i, o := range x {
			if i > 0 {
				b.WriteString(itemSep)
			}
			writeJSON(b, o, itemSep, keySep)
		}
		b.WriteByte(']')
	case object:
		b.WriteByte('{')
		for i, m := range x {
			if i > 0 {
				b.WriteString(itemSep)
			}
			writeJSONString(b, m.key)
			b.WriteString(keySep)
			writeJSON(b, m.value, itemSep, keySep)
		}
		b.WriteByte('}')
	}
}

func writeJSONString(b *strings.Builder, s string) {
	const hex = "0123456789abcdef"
	unit := func(u uint16) {
		b.WriteString(`\u`)
		b.WriteByte(hex[u>>12])
		b.WriteByte(hex[u>>8&15])
		b.WriteByte(hex[u>>4&15])
		b.WriteByte(hex[u&15])
	}
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r >= 0x20 && r <= 0x7e:
			b.WriteByte(byte(r))
		case r > 0xffff && r != utf8.RuneError:
			hi, lo := utf16.EncodeRune(r)
			unit(uint16(hi))
			unit(uint16(lo))
		default:
			unit(uint16(r))
		}
	}
	b.WriteByte('"')
}

func spacedJSON(v any) []byte {
	var b strings.Builder
	writeJSON(&b, v, ", ", ": ")
	return []byte(b.String())
}

func compactJSON(v any) []byte {
	var b strings.Builder
	writeJSON(&b, v, ",", ":")
	return []byte(b.String())
}

func signJWT(method jwt.SigningMethod, key any, keyID string, claims object) (string, error) {
	header := object{{"typ", "JWT"}, {"alg", method.Alg()}}
	if keyID != "" {
		header = append(header, member{"kid", keyID})
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(spacedJSON(header)) + "." + enc.EncodeToString(spacedJSON(claims))
	sig, err := method.Sign(signing, key)
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

func parsePrivateKey(text string) (any, error) {
	block, _ := pem.Decode([]byte(text))
	if block == nil {
		return nil, errors.New("private key is not PEM encoded")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		return x509.ParseECPrivateKey(block.Bytes)
	}
	return x509.ParsePKCS8PrivateKey(block.Bytes)
}

func parseRSAKey(text string) (*rsa.PrivateKey, error) {
	key, err := parsePrivateKey(text)
	if err != nil {
		return nil, errors.New("private key could not be read")
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not an RSA key")
	}
	return rsaKey, nil
}

func parseECKey(text string) (*ecdsa.PrivateKey, error) {
	key, err := parsePrivateKey(text)
	if err != nil {
		return nil, errors.New("private key could not be read")
	}
	ecKey, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not an elliptic curve key")
	}
	return ecKey, nil
}

func readBounded(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if info.Size() > fileLimit {
		return nil, errors.New("file is larger than 1 MiB")
	}
	return os.ReadFile(path)
}
