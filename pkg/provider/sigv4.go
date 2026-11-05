package provider

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	sigV4Algorithm       = "AWS4-HMAC-SHA256"
	sigV4TimeFormat      = "20060102T150405Z"
	sigV4DateFormat      = "20060102"
	sigV4Terminator      = "aws4_request"
	sigV4EmptyPayloadSHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// Headers botocore never signs because intermediaries rewrite or drop them
var sigV4UnsignedHeaders = map[string]bool{"expect": true, "transfer-encoding": true, "user-agent": true, "x-amzn-trace-id": true, "connection": true}

// One AWS access key set with its expiry, zero Expires never expires
type awsCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Expires         time.Time
	Source          string
}

// Reports a usable key pair
func (c awsCredentials) valid() bool {
	return c.AccessKeyID != "" && c.SecretAccessKey != ""
}

// Pieces of one signature kept for verification and tests
type sigV4Result struct {
	CanonicalRequest string
	StringToSign     string
	SignedHeaders    string
	Scope            string
	Signature        string
	Authorization    string
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

// Derives the date, region, service and terminator HMAC chain from the secret key
func sigV4SigningKey(secret, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), date)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	return hmacSHA256(kService, sigV4Terminator)
}

// Percent encodes every byte outside the RFC 3986 unreserved set, slashes kept when asked
func sigV4Escape(s string, keepSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && keepSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte("0123456789ABCDEF"[c>>4])
			b.WriteByte("0123456789ABCDEF"[c&15])
		}
	}
	return b.String()
}

// RFC 3986 dot segment removal that also folds consecutive slashes as AWS services require
func sigV4RemoveDotSegments(path string) string {
	if path == "" {
		return "/"
	}
	var out []string
	for _, seg := range strings.Split(path, "/") {
		switch seg {
		case "", ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, seg)
		}
	}
	first, last := "", ""
	if strings.HasPrefix(path, "/") {
		first = "/"
	}
	if strings.HasSuffix(path, "/") && len(out) > 0 {
		last = "/"
	}
	joined := first + strings.Join(out, "/") + last
	if joined == "" {
		return "/"
	}
	return joined
}

// Canonical URI of the escaped request path, encoded a second time as non S3 services expect
func sigV4CanonicalURI(u *url.URL) string {
	return sigV4Escape(sigV4RemoveDotSegments(u.EscapedPath()), true)
}

// Sorted, re-encoded query pairs of the raw query string
func sigV4CanonicalQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	type pair struct{ k, v string }
	var pairs []pair
	for _, part := range strings.Split(rawQuery, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		if dk, err := url.QueryUnescape(k); err == nil {
			k = dk
		}
		if dv, err := url.QueryUnescape(v); err == nil {
			v = dv
		}
		pairs = append(pairs, pair{sigV4Escape(k, false), sigV4Escape(v, false)})
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

// Host the request goes to with the scheme default port dropped
func sigV4Host(req *http.Request) string {
	host := req.Host
	if host == "" && req.URL != nil {
		host = req.URL.Host
	}
	scheme := ""
	if req.URL != nil {
		scheme = strings.ToLower(req.URL.Scheme)
	}
	if (scheme == "https" && strings.HasSuffix(host, ":443")) || (scheme == "http" && strings.HasSuffix(host, ":80")) {
		host = host[:strings.LastIndex(host, ":")]
	}
	return host
}

// Lowercased header names and whitespace folded values of the signed header set plus host
func sigV4HeadersToSign(req *http.Request, only map[string]bool) ([]string, map[string]string) {
	values := map[string]string{}
	for name, vals := range req.Header {
		lname := strings.ToLower(name)
		if sigV4UnsignedHeaders[lname] || (only != nil && !only[lname]) {
			continue
		}
		folded := make([]string, 0, len(vals))
		for _, v := range vals {
			folded = append(folded, strings.Join(strings.Fields(v), " "))
		}
		if prior, ok := values[lname]; ok {
			values[lname] = prior + "," + strings.Join(folded, ",")
		} else {
			values[lname] = strings.Join(folded, ",")
		}
	}
	if _, ok := values["host"]; !ok && (only == nil || only["host"]) {
		values["host"] = sigV4Host(req)
	}
	names := make([]string, 0, len(values))
	for n := range values {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, values
}

// Builds the canonical request, string to sign and signature for a request whose date headers are already set
func sigV4Compute(req *http.Request, payloadHash string, creds awsCredentials, service, region string, ts time.Time, only map[string]bool) sigV4Result {
	names, values := sigV4HeadersToSign(req, only)
	var canonHeaders strings.Builder
	for _, n := range names {
		canonHeaders.WriteString(n + ":" + values[n] + "\n")
	}
	signed := strings.Join(names, ";")
	canonical := strings.Join([]string{
		strings.ToUpper(req.Method), sigV4CanonicalURI(req.URL), sigV4CanonicalQuery(req.URL.RawQuery),
		canonHeaders.String(), signed, payloadHash,
	}, "\n")
	date := ts.UTC().Format(sigV4DateFormat)
	scope := date + "/" + region + "/" + service + "/" + sigV4Terminator
	toSign := strings.Join([]string{sigV4Algorithm, ts.UTC().Format(sigV4TimeFormat), scope, sha256Hex([]byte(canonical))}, "\n")
	sig := hex.EncodeToString(hmacSHA256(sigV4SigningKey(creds.SecretAccessKey, date, region, service), toSign))
	return sigV4Result{
		CanonicalRequest: canonical, StringToSign: toSign, SignedHeaders: signed, Scope: scope, Signature: sig,
		Authorization: sigV4Algorithm + " Credential=" + creds.AccessKeyID + "/" + scope + ", SignedHeaders=" + signed + ", Signature=" + sig,
	}
}

// Payload hash from an explicit X-Amz-Content-Sha256 header, else the SHA256 of the body
func sigV4PayloadHash(req *http.Request, body []byte) string {
	if v := req.Header.Get("X-Amz-Content-Sha256"); v != "" {
		return v
	}
	if len(body) == 0 {
		return sigV4EmptyPayloadSHA
	}
	return sha256Hex(body)
}

// Signs the request in place with X-Amz-Date, the session token header and the Authorization header
func sigV4Sign(req *http.Request, body []byte, creds awsCredentials, service, region string, now time.Time) (sigV4Result, error) {
	if !creds.valid() {
		return sigV4Result{}, errors.New("provider: sigv4 signing needs an access key id and secret access key")
	}
	if req.URL == nil {
		return sigV4Result{}, errors.New("provider: sigv4 signing needs a request URL")
	}
	ts := now.UTC()
	req.Header.Del("Authorization")
	req.Header.Del("X-Amz-Security-Token")
	if req.Header.Get("Date") != "" {
		req.Header.Del("Date")
		req.Header.Del("X-Amz-Date")
		req.Header.Set("Date", ts.Format(http.TimeFormat))
	} else {
		req.Header.Set("X-Amz-Date", ts.Format(sigV4TimeFormat))
	}
	if creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", creds.SessionToken)
	}
	res := sigV4Compute(req, sigV4PayloadHash(req, body), creds, service, region, ts, nil)
	req.Header.Set("Authorization", res.Authorization)
	return res, nil
}

// Reads the request body into memory and rewinds it so the signature covers the exact bytes sent
func sigV4BufferBody(req *http.Request) ([]byte, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}
	data, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return nil, err
	}
	req.Body = io.NopCloser(bytes.NewReader(data))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	req.ContentLength = int64(len(data))
	return data, nil
}

// Round tripper signing every outgoing request with credentials resolved per request
type sigV4RoundTripper struct {
	base    http.RoundTripper
	creds   awsCredentialProvider
	service string
	region  string
	drop    []string
	now     func() time.Time
}

// RoundTrip clones the request, drops the sentinel auth headers and signs the clone
func (t *sigV4RoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	creds, err := t.creds.Retrieve(ctx)
	if err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	clone := req.Clone(ctx)
	for _, h := range t.drop {
		clone.Header.Del(h)
	}
	body, err := sigV4BufferBody(clone)
	if err != nil {
		return nil, err
	}
	now := time.Now
	if t.now != nil {
		now = t.now
	}
	if _, err := sigV4Sign(clone, body, creds, t.service, t.region, now()); err != nil {
		return nil, err
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}
