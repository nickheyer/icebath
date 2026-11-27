// Package aws resolves AWS credentials the way the AWS CLI and SDKs do, and signs requests with them by SigV4
package aws

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	algorithm  = "AWS4-HMAC-SHA256"
	timeFormat = "20060102T150405Z"
	dateFormat = "20060102"
	terminator = "aws4_request"
	emptySHA   = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	// Date header a request that carries one is signed with
	httpDate = "Mon, 02 Jan 2006 15:04:05 -0000"
)

// Headers never signed, since what sits between rewrites or drops them
var unsigned = map[string]bool{"baggage": true, "connection": true, "expect": true, "keep-alive": true, "proxy-authenticate": true,
	"proxy-authorization": true, "te": true, "traceparent": true, "tracestate": true, "trailer": true, "transfer-encoding": true, "upgrade": true,
	"user-agent": true, "x-amzn-trace-id": true}

// One set of AWS keys, with when it expires; zero Expires never does
type Keys struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Expires         time.Time
	// Where in the chain the keys came from, such as env, sso or iam-role
	Source string
}

// Whether the keys can sign
func (k Keys) Valid() bool {
	return k.AccessKeyID != "" && k.SecretAccessKey != ""
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// Every byte outside the RFC 3986 unreserved set percent encoded, slashes kept when asked
func escape(s string, keepSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~', c == '/' && keepSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte("0123456789ABCDEF"[c>>4])
			b.WriteByte("0123456789ABCDEF"[c&15])
		}
	}
	return b.String()
}

// Path with its dot segments removed and runs of slashes folded, as AWS services read it
func clean(path string) string {
	if path == "" {
		return "/"
	}
	var out []string
	for _, segment := range strings.Split(path, "/") {
		switch segment {
		case "", ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, segment)
		}
	}
	first, last := "", ""
	if strings.HasPrefix(path, "/") {
		first = "/"
	}
	if strings.HasSuffix(path, "/") && len(out) > 0 {
		last = "/"
	}
	if joined := first + strings.Join(out, "/") + last; joined != "" {
		return joined
	}
	return "/"
}

// Sorted, encoded again pairs of the query
func query(raw string) string {
	if raw == "" {
		return ""
	}
	type pair struct{ k, v string }
	var pairs []pair
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		if decoded, err := url.QueryUnescape(k); err == nil {
			k = decoded
		}
		if decoded, err := url.QueryUnescape(v); err == nil {
			v = decoded
		}
		pairs = append(pairs, pair{escape(k, false), escape(v, false)})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = p.k + "=" + p.v
	}
	return strings.Join(parts, "&")
}

// Host the request goes to, the scheme's own port left out
func host(req *http.Request) string {
	h := req.Host
	if h == "" {
		h = req.URL.Host
	}
	scheme := strings.ToLower(req.URL.Scheme)
	if scheme == "https" && strings.HasSuffix(h, ":443") || scheme == "http" && strings.HasSuffix(h, ":80") {
		h = h[:strings.LastIndex(h, ":")]
	}
	return h
}

// Signs the request in place: its date, the session token and the Authorization header over the exact body given
func Sign(req *http.Request, body []byte, keys Keys, service, region string, now time.Time) error {
	if !keys.Valid() {
		return errors.New("AWS requests need an access key ID and a secret access key, and these credentials lack one.")
	}
	ts := now.UTC()
	req.Header.Del("Authorization")
	req.Header.Del("X-Amz-Security-Token")
	if req.Header.Get("Date") != "" {
		req.Header.Del("X-Amz-Date")
		req.Header.Set("Date", ts.Format(httpDate))
	} else {
		req.Header.Set("X-Amz-Date", ts.Format(timeFormat))
	}
	if keys.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", keys.SessionToken)
	}
	payload := req.Header.Get("X-Amz-Content-Sha256")
	switch {
	case payload != "":
	case len(body) == 0:
		payload = emptySHA
	default:
		payload = sha256Hex(body)
	}
	values := map[string]string{}
	for name, vals := range req.Header {
		lower := strings.ToLower(name)
		if unsigned[lower] {
			continue
		}
		folded := make([]string, len(vals))
		for i, v := range vals {
			folded[i] = strings.Join(strings.Fields(v), " ")
		}
		values[lower] = strings.Join(folded, ",")
	}
	if _, ok := values["host"]; !ok {
		values["host"] = host(req)
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	var headers strings.Builder
	for _, name := range names {
		headers.WriteString(name + ":" + values[name] + "\n")
	}
	signed := strings.Join(names, ";")
	canonical := strings.Join([]string{strings.ToUpper(req.Method), escape(clean(req.URL.EscapedPath()), true), query(req.URL.RawQuery),
		headers.String(), signed, payload}, "\n")
	date := ts.Format(dateFormat)
	scope := date + "/" + region + "/" + service + "/" + terminator
	toSign := strings.Join([]string{algorithm, ts.Format(timeFormat), scope, sha256Hex([]byte(canonical))}, "\n")
	key := hmacSHA256(hmacSHA256(hmacSHA256(hmacSHA256([]byte("AWS4"+keys.SecretAccessKey), date), region), service), terminator)
	signature := hex.EncodeToString(hmacSHA256(key, toSign))
	req.Header.Set("Authorization", algorithm+" Credential="+keys.AccessKeyID+"/"+scope+", SignedHeaders="+signed+", Signature="+signature)
	return nil
}
